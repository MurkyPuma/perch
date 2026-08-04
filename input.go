package main

import "unicode/utf8"

// Input tokenizer. In live mode we forward bytes to the app untouched except
// for three intercepts: Ctrl-Space (palette), Ctrl-\ (detach), and SGR mouse
// reports (which exist because the client terminal has mouse mode on, not
// because the app asked). In palette/scroll modes we parse keys properly.

type evKind int

const (
	evRaw evKind = iota
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
	// expect: ESC [ < params M|m
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

// tokenizeLive splits input into raw chunks plus intercepted events.
// rest holds an incomplete trailing sequence to prepend to the next read.
func tokenizeLive(p []byte) (evs []event, rest []byte) {
	rawStart := 0
	flush := func(end int) {
		if end > rawStart {
			evs = append(evs, event{kind: evRaw, data: p[rawStart:end]})
		}
	}
	for i := 0; i < len(p); {
		b := p[i]
		switch b {
		case 0x00:
			flush(i)
			evs = append(evs, event{kind: evKey, key: keyCtrlSpace})
			i++
			rawStart = i
		case 0x1c:
			flush(i)
			evs = append(evs, event{kind: evKey, key: keyCtrlBackslash})
			i++
			rawStart = i
		case 0x1b:
			me, n := parseMouse(p[i:])
			if n > 0 {
				flush(i)
				evs = append(evs, event{kind: evMouse, mouse: me})
				i += n
				rawStart = i
			} else if n == 0 {
				// incomplete possible mouse sequence at the end of the chunk
				flush(i)
				return evs, append([]byte{}, p[i:]...)
			} else {
				i++
			}
		default:
			i++
		}
	}
	flush(len(p))
	return evs, nil
}

// tokenizeKeys parses input into discrete key/rune/mouse events for the
// palette and scroll modes. Unrecognized sequences are dropped.
func tokenizeKeys(p []byte) []event {
	var evs []event
	for i := 0; i < len(p); {
		b := p[i]
		if b == 0x1b {
			if i == len(p)-1 {
				evs = append(evs, event{kind: evKey, key: keyEsc})
				break
			}
			me, n := parseMouse(p[i:])
			if n > 0 {
				evs = append(evs, event{kind: evMouse, mouse: me})
				i += n
				continue
			}
			k, n2 := parseEscKey(p[i:])
			if n2 > 0 {
				if k != keyNone {
					evs = append(evs, event{kind: evKey, key: k})
				}
				i += n2
				continue
			}
			evs = append(evs, event{kind: evKey, key: keyEsc})
			i++
			continue
		}
		if k := ctrlKey(b); k != keyNone {
			evs = append(evs, event{kind: evKey, key: k})
			i++
			continue
		}
		if b < 0x20 {
			i++
			continue
		}
		r, sz := utf8.DecodeRune(p[i:])
		if r == utf8.RuneError && sz == 1 {
			i++
			continue
		}
		evs = append(evs, event{kind: evRune, r: r})
		i += sz
	}
	return evs
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
// Returns consumed==0 when p is not a recognizable prefix.
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
