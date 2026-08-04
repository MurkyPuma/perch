package main

import "unicode/utf8"

// Input tokenizer. In live mode we forward bytes to the app untouched except
// for three intercepts: Ctrl-Space (palette), Ctrl-\ (detach), and SGR mouse
// reports (which exist because the client terminal has mouse mode on, not
// because the app asked). In palette/scroll modes we parse keys properly.
//
// Events are consumed ONE at a time so a key that changes the client's mode
// (e.g. Ctrl-Space) routes the rest of the same chunk under the new mode.

type evKind int

const (
	evNone evKind = iota
	evRaw
	evKey
	evRune
	evMouse
)

type key int

const (
	keyNone key = iota
	keyEsc
	keyEnter
	keyBackspace
	keyUp
	keyDown
	keyLeft
	keyRight
	keyPgUp
	keyPgDn
	keyHome
	keyEnd
	keyCtrlSpace
	keyCtrlBackslash
	keyCtrlC
	keyCtrlD
	keyCtrlN
	keyCtrlP
	keyCtrlU
	keyCtrlX
)

type mouseEv struct {
	btn   int
	x, y  int // 1-based terminal coordinates
	press bool
	raw   []byte
}

const (
	mouseWheelUp   = 64
	mouseWheelDown = 65
)

type event struct {
	kind  evKind
	data  []byte // evRaw
	key   key    // evKey
	r     rune   // evRune
	mouse mouseEv
}

// parseMouse parses an SGR mouse report starting at p[0]=='\x1b'. Returns the
// event and bytes consumed, or consumed==0 if p is a valid-so-far prefix that
// needs more bytes, or consumed==-1 if this is not a mouse sequence.
func parseMouse(p []byte) (mouseEv, int) {
	prefix := []byte{0x1b, '[', '<'}
	for i, b := range prefix {
		if i >= len(p) {
			return mouseEv{}, 0
		}
		if p[i] != b {
			return mouseEv{}, -1
		}
	}
	nums := [3]int{}
	ni := 0
	for i := 3; i < len(p); i++ {
		b := p[i]
		switch {
		case b >= '0' && b <= '9':
			nums[ni] = nums[ni]*10 + int(b-'0')
		case b == ';':
			ni++
			if ni > 2 {
				return mouseEv{}, -1
			}
		case b == 'M' || b == 'm':
			return mouseEv{btn: nums[0], x: nums[1], y: nums[2], press: b == 'M', raw: p[:i+1]}, i + 1
		default:
			return mouseEv{}, -1
		}
		if i-3 > 24 {
			return mouseEv{}, -1
		}
	}
	return mouseEv{}, 0 // incomplete
}

// nextEventLive extracts the next event under live-mode rules.
// incomplete=true means p starts with a possibly-unfinished sequence: stash it
// and wait for more bytes.
func nextEventLive(p []byte) (ev event, n int, incomplete bool) {
	for i := 0; i < len(p); {
		b := p[i]
		switch b {
		case 0x00:
			if i > 0 {
				return event{kind: evRaw, data: p[:i]}, i, false
			}
			return event{kind: evKey, key: keyCtrlSpace}, 1, false
		case 0x1c:
			if i > 0 {
				return event{kind: evRaw, data: p[:i]}, i, false
			}
			return event{kind: evKey, key: keyCtrlBackslash}, 1, false
		case 0x1b:
			me, mn := parseMouse(p[i:])
			if mn > 0 {
				if i > 0 {
					return event{kind: evRaw, data: p[:i]}, i, false
				}
				return event{kind: evMouse, mouse: me}, mn, false
			}
			if mn == 0 { // incomplete possible mouse sequence at chunk end
				if i > 0 {
					return event{kind: evRaw, data: p[:i]}, i, false
				}
				return event{}, 0, true
			}
			i++
		default:
			i++
		}
	}
	return event{kind: evRaw, data: p}, len(p), false
}

// nextEventKeys extracts one parsed key/rune/mouse event (palette and scroll
// modes). Always consumes at least one byte; kind evNone means "ignore".
func nextEventKeys(p []byte) (ev event, n int) {
	b := p[0]
	if b == 0x1b {
		if len(p) == 1 {
			return event{kind: evKey, key: keyEsc}, 1
		}
		if me, mn := parseMouse(p); mn > 0 {
			return event{kind: evMouse, mouse: me}, mn
		}
		if k, kn := parseEscKey(p); kn > 0 {
			if k != keyNone {
				return event{kind: evKey, key: k}, kn
			}
			return event{kind: evNone}, kn
		}
		return event{kind: evKey, key: keyEsc}, 1
	}
	if k := ctrlKey(b); k != keyNone {
		return event{kind: evKey, key: k}, 1
	}
	if b < 0x20 {
		return event{kind: evNone}, 1
	}
	r, sz := utf8.DecodeRune(p)
	if r == utf8.RuneError && sz == 1 {
		return event{kind: evNone}, 1
	}
	return event{kind: evRune, r: r}, sz
}

func ctrlKey(b byte) key {
	switch b {
	case 0x00:
		return keyCtrlSpace
	case 0x1c:
		return keyCtrlBackslash
	case '\r', '\n':
		return keyEnter
	case 0x7f, 0x08:
		return keyBackspace
	case 0x03:
		return keyCtrlC
	case 0x04:
		return keyCtrlD
	case 0x0e:
		return keyCtrlN
	case 0x10:
		return keyCtrlP
	case 0x15:
		return keyCtrlU
	case 0x18:
		return keyCtrlX
	}
	return keyNone
}

// parseEscKey parses ESC [ ... and ESC O ... key sequences.
// Returns consumed==0 when p cannot be decided yet.
func parseEscKey(p []byte) (key, int) {
	if len(p) < 2 {
		return keyNone, 0
	}
	switch p[1] {
	case 'O':
		if len(p) < 3 {
			return keyNone, 2
		}
		switch p[2] {
		case 'A':
			return keyUp, 3
		case 'B':
			return keyDown, 3
		case 'C':
			return keyRight, 3
		case 'D':
			return keyLeft, 3
		case 'H':
			return keyHome, 3
		case 'F':
			return keyEnd, 3
		}
		return keyNone, 3
	case '[':
		j := 2
		for j < len(p) && !(p[j] >= 0x40 && p[j] <= 0x7e) {
			j++
			if j-2 > 16 {
				return keyNone, j
			}
		}
		if j >= len(p) {
			return keyNone, len(p)
		}
		final := p[j]
		params := string(p[2:j])
		n := j + 1
		switch final {
		case 'A':
			return keyUp, n
		case 'B':
			return keyDown, n
		case 'C':
			return keyRight, n
		case 'D':
			return keyLeft, n
		case 'H':
			return keyHome, n
		case 'F':
			return keyEnd, n
		case '~':
			switch params {
			case "5":
				return keyPgUp, n
			case "6":
				return keyPgDn, n
			case "1", "7":
				return keyHome, n
			case "4", "8":
				return keyEnd, n
			}
			return keyNone, n
		}
		return keyNone, n
	}
	return keyNone, 1
}

// translateArrows rewrites CSI arrow/home/end sequences to their SS3
// (application cursor mode) variants, for apps that enabled DECCKM.
func translateArrows(p []byte) []byte {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); {
		if p[i] == 0x1b && i+2 < len(p) && p[i+1] == '[' {
			switch p[i+2] {
			case 'A', 'B', 'C', 'D', 'H', 'F':
				out = append(out, 0x1b, 'O', p[i+2])
				i += 3
				continue
			}
		}
		out = append(out, p[i])
		i++
	}
	return out
}
