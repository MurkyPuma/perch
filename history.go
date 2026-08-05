package main

import (
	"fmt"
	"strings"
)

// History captures finished lines of terminal output for scrollback, preserving
// SGR color state, by running a small state machine over the raw PTY stream.
// It is an approximation: cursor-addressed rewrites (progress bars using \r,
// Ink-style repaints using cursor-up) keep only their final state, and
// alt-screen output (vim, less) is skipped, which matches what you want from
// shell scrollback.
//
// The same pass sniffs DECSET/DECRST private modes so the server knows whether
// the foreground app is on the alt screen or has requested mouse reporting.

type histLine struct {
	s string // line content with embedded SGR sequences
	w int    // printable width (cells, tabs expanded)
}

// sgrState tracks the terminal's current text attributes so a line that begins
// mid-style can reopen it with one canonical, always-well-formed sequence.
// (Naively accumulating raw SGR strings and truncating them by bytes cuts
// escape sequences in half and leaks fragments like ";215;119;87m" as text.)
type sgrState struct {
	fg, bg                                               string // "31", "38;5;114", "38;2;r;g;b"; "" = default
	bold, dim, italic, underline, blink, reverse, strike bool
}

func (s *sgrState) isDefault() bool { return *s == sgrState{} }

// emit returns one canonical sequence reproducing the state from default.
func (s *sgrState) emit() string {
	if s.isDefault() {
		return ""
	}
	parts := []string{"0"}
	if s.bold {
		parts = append(parts, "1")
	}
	if s.dim {
		parts = append(parts, "2")
	}
	if s.italic {
		parts = append(parts, "3")
	}
	if s.underline {
		parts = append(parts, "4")
	}
	if s.blink {
		parts = append(parts, "5")
	}
	if s.reverse {
		parts = append(parts, "7")
	}
	if s.strike {
		parts = append(parts, "9")
	}
	if s.fg != "" {
		parts = append(parts, s.fg)
	}
	if s.bg != "" {
		parts = append(parts, s.bg)
	}
	return "\x1b[" + strings.Join(parts, ";") + "m"
}

func (s *sgrState) apply(p []int) {
	for i := 0; i < len(p); i++ {
		a := p[i]
		switch {
		case a == 0:
			*s = sgrState{}
		case a == 1:
			s.bold = true
		case a == 2:
			s.dim = true
		case a == 3:
			s.italic = true
		case a == 4:
			s.underline = true
		case a == 5 || a == 6:
			s.blink = true
		case a == 7:
			s.reverse = true
		case a == 9:
			s.strike = true
		case a == 21 || a == 22:
			s.bold, s.dim = false, false
		case a == 23:
			s.italic = false
		case a == 24:
			s.underline = false
		case a == 25 || a == 26:
			s.blink = false
		case a == 27:
			s.reverse = false
		case a == 29:
			s.strike = false
		case a == 38 || a == 48:
			frag := ""
			if i+2 < len(p) && p[i+1] == 5 {
				frag = fmt.Sprintf("%d;5;%d", a, p[i+2])
				i += 2
			} else if i+4 < len(p) && p[i+1] == 2 {
				frag = fmt.Sprintf("%d;2;%d;%d;%d", a, p[i+2], p[i+3], p[i+4])
				i += 4
			}
			if a == 38 {
				s.fg = frag
			} else {
				s.bg = frag
			}
		case a == 39:
			s.fg = ""
		case a == 49:
			s.bg = ""
		case (a >= 30 && a <= 37) || (a >= 90 && a <= 97):
			s.fg = fmt.Sprintf("%d", a)
		case (a >= 40 && a <= 47) || (a >= 100 && a <= 107):
			s.bg = fmt.Sprintf("%d", a)
		}
	}
}

type History struct {
	lines   []histLine
	max     int
	trimmed int // count of lines dropped from the front; keeps indices absolute

	cur      []byte // current line under construction
	curW     int
	startSGR sgrState // SGR state active when the current line began
	sgr      sgrState // current SGR state
	crSeen   bool     // saw \r; next printable rewrites the line

	state int
	seq   []byte // escape sequence under construction

	// Flags sniffed from the stream for the session to consume.
	AltScreen  bool
	MouseOn    bool
	CursorKeys bool // DECCKM: app wants ESC O A style arrows
	Paste      bool // app enabled bracketed paste
}

const (
	stGround = iota
	stEsc
	stCSI
	stOSC
	stOSCEsc
	stCharset
)

const tabWidth = 8

func newHistory(max int) *History {
	return &History{max: max}
}

// Absolute line indices: stable as the ring trims, so a scroll anchor does not
// jump when old lines are dropped.
func (h *History) Total() int              { return h.trimmed + len(h.lines) }
func (h *History) First() int              { return h.trimmed }
func (h *History) LineAt(abs int) histLine { return h.lines[abs-h.trimmed] }

// DocLen/DocLine expose the history plus the in-progress line as one
// scrollable document, so the bottom of the scroll view matches the live
// prompt line instead of cutting it off.
func (h *History) DocLen() int { return h.trimmed + len(h.lines) + 1 }

func (h *History) DocLine(abs int) histLine {
	if i := abs - h.trimmed; i < len(h.lines) {
		return h.lines[i]
	}
	line := string(h.cur)
	if pre := h.startSGR.emit(); line != "" {
		if pre != "" {
			line = pre + line
		}
		if pre != "" || strings.ContainsRune(line, 0x1b) {
			line += "\x1b[0m"
		}
	}
	return histLine{s: line, w: h.curW}
}

func (h *History) pushLine() {
	line := string(h.cur)
	styled := !h.startSGR.isDefault()
	if !styled {
		for _, b := range h.cur {
			if b == 0x1b {
				styled = true
				break
			}
		}
	}
	if line != "" {
		if pre := h.startSGR.emit(); pre != "" {
			line = pre + line
		}
		if styled {
			line += "\x1b[0m"
		}
	}
	h.lines = append(h.lines, histLine{s: line, w: h.curW})
	if len(h.lines) > h.max {
		drop := len(h.lines) - h.max
		h.lines = h.lines[drop:]
		h.trimmed += drop
	}
	h.cur = h.cur[:0]
	h.curW = 0
	h.startSGR = h.sgr
	h.crSeen = false
}

func (h *History) resetLine() {
	h.cur = h.cur[:0]
	h.curW = 0
	h.startSGR = h.sgr
}

// popLines handles cursor-up: the app is moving back to repaint lines it
// already drew (spinners, Ink/Claude Code-style live areas). Forget the
// current line and the last n completed ones; the app redraws them next.
func (h *History) popLines(n int) {
	h.resetLine()
	h.crSeen = false
	for n > 0 && len(h.lines) > 0 {
		h.lines = h.lines[:len(h.lines)-1]
		n--
	}
}

// Feed consumes a chunk of PTY output.
func (h *History) Feed(p []byte) {
	for _, b := range p {
		switch h.state {
		case stGround:
			h.ground(b)
		case stEsc:
			h.esc(b)
		case stCSI:
			h.seq = append(h.seq, b)
			if b >= 0x40 && b <= 0x7e {
				h.handleCSI()
				h.state = stGround
			} else if len(h.seq) > 64 {
				h.state = stGround // malformed, bail
			}
		case stOSC:
			if b == 0x07 {
				h.state = stGround
			} else if b == 0x1b {
				h.state = stOSCEsc
			}
		case stOSCEsc:
			// ESC \ terminates OSC; anything else means a new escape started
			if b == '\\' {
				h.state = stGround
			} else {
				h.state = stEsc
				h.esc(b)
			}
		case stCharset:
			h.state = stGround
		}
	}
}

func (h *History) ground(b byte) {
	switch {
	case b == 0x1b:
		h.state = stEsc
	case b == '\n':
		h.pushLine()
	case b == '\r':
		h.crSeen = true
	case b == '\t':
		if !h.AltScreen {
			n := tabWidth - h.curW%tabWidth
			for i := 0; i < n; i++ {
				h.cur = append(h.cur, ' ')
			}
			h.curW += n
		}
	case b == 0x08: // backspace: drop the last rune
		if !h.AltScreen && len(h.cur) > 0 {
			i := len(h.cur) - 1
			for i > 0 && h.cur[i]&0xc0 == 0x80 {
				i--
			}
			h.cur = h.cur[:i]
			if h.curW > 0 {
				h.curW--
			}
		}
	case b < 0x20 || b == 0x7f:
		// other control chars: ignore for history
	default:
		if h.AltScreen {
			return
		}
		if h.crSeen {
			h.resetLine()
			h.crSeen = false
		}
		h.cur = append(h.cur, b)
		// count width only on non-continuation bytes (UTF-8 aware, width 1 assumed)
		if b&0xc0 != 0x80 {
			h.curW++
		}
	}
}

func (h *History) esc(b byte) {
	switch b {
	case '[':
		h.seq = h.seq[:0]
		h.state = stCSI
	case ']':
		h.state = stOSC
	case '(', ')', '*', '+', '#':
		h.state = stCharset
	case 'c': // RIS full reset
		h.sgr = sgrState{}
		h.resetLine()
		h.state = stGround
	default:
		h.state = stGround
	}
}

func (h *History) handleCSI() {
	seq := h.seq
	if len(seq) == 0 {
		return
	}
	final := seq[len(seq)-1]
	params := string(seq[:len(seq)-1])
	ints := splitParams(params)
	p1 := 1
	if len(ints) > 0 && ints[0] > 0 {
		p1 = ints[0]
	}
	switch final {
	case 'm':
		h.handleSGR(params, ints)
	case 'h', 'l':
		if len(params) > 0 && params[0] == '?' {
			h.handleDECSET(params[1:], final == 'h')
		}
	case 'A', 'F': // cursor up: app repaints its last n lines
		if !h.AltScreen {
			h.popLines(p1)
		}
	case 'C': // cursor forward: spacing without spaces (Ink, column layouts)
		if !h.AltScreen && !h.crSeen {
			if p1 > 500 {
				p1 = 500
			}
			for i := 0; i < p1; i++ {
				h.cur = append(h.cur, ' ')
			}
			h.curW += p1
		}
	case 'G': // column absolute
		if !h.AltScreen {
			if p1 <= 1 {
				h.crSeen = true
			} else if pad := p1 - 1 - h.curW; pad > 0 {
				for i := 0; i < pad; i++ {
					h.cur = append(h.cur, ' ')
				}
				h.curW += pad
			}
		}
	case 'K':
		if !h.AltScreen && params == "2" {
			h.resetLine()
		}
	}
}

func (h *History) handleSGR(params string, ints []int) {
	if len(ints) == 0 {
		ints = []int{0}
	}
	h.sgr.apply(ints)
	if !h.AltScreen {
		// keep the app's own (complete) sequence in the line body
		h.cur = append(h.cur, []byte("\x1b["+params+"m")...)
	}
}

func (h *History) handleDECSET(params string, on bool) {
	for _, p := range splitParams(params) {
		switch p {
		case 1:
			h.CursorKeys = on
		case 1049, 1047, 47:
			h.AltScreen = on
		case 1000, 1002, 1003:
			h.MouseOn = on
		case 2004:
			h.Paste = on
		}
	}
}

// splitParams parses "38;2;215;119;87" into ints; empty segments become 0
// (";31" means "0;31" per ECMA-48). Non-digit bytes are skipped.
func splitParams(s string) []int {
	if s == "" {
		return nil
	}
	var out []int
	n := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ';' {
			out = append(out, n)
			n = 0
			continue
		}
		if s[i] >= '0' && s[i] <= '9' {
			n = n*10 + int(s[i]-'0')
		}
	}
	return out
}
