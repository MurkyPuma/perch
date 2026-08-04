package main

// History captures finished lines of terminal output for scrollback, preserving
// SGR color state, by running a small state machine over the raw PTY stream.
// It is an approximation: cursor-addressed rewrites (progress bars using \r)
// keep only their final state, and alt-screen output (vim, less) is skipped,
// which matches what you want from shell scrollback.
//
// The same pass sniffs DECSET/DECRST private modes so the server knows whether
// the foreground app is on the alt screen or has requested mouse reporting.

type histLine struct {
	s string // line content with embedded SGR sequences
	w int    // printable width (cells, tabs expanded)
}

type History struct {
	lines   []histLine
	max     int
	trimmed int // count of lines dropped from the front; keeps indices absolute

	cur      []byte // current line under construction
	curW     int
	startSGR string // SGR state active when the current line began
	sgr      string // accumulated SGR state ("" means default)
	crSeen   bool   // saw \r; next printable rewrites the line

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

func (h *History) pushLine() {
	line := string(h.cur)
	if h.startSGR != "" && line != "" {
		line = h.startSGR + line
	}
	if line != "" {
		line += "\x1b[0m"
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
		h.sgr = ""
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
	switch final {
	case 'm':
		h.handleSGR(params)
	case 'h', 'l':
		if len(params) > 0 && params[0] == '?' {
			h.handleDECSET(params[1:], final == 'h')
		}
	case 'K':
		if !h.AltScreen && (params == "" || params == "0") {
			// erase to end of line: current buffer already ends here, nothing to do
		} else if !h.AltScreen && params == "2" {
			h.resetLine()
		}
	}
}

func (h *History) handleSGR(params string) {
	if params == "" || params == "0" {
		h.sgr = ""
	} else {
		h.sgr += "\x1b[" + params + "m"
		if len(h.sgr) > 256 {
			// runaway accumulation; keep the tail, it holds the latest state
			h.sgr = h.sgr[len(h.sgr)-256:]
		}
	}
	if !h.AltScreen {
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

func splitParams(s string) []int {
	var out []int
	n, has := 0, false
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ';' {
			if has {
				out = append(out, n)
			}
			n, has = 0, false
			continue
		}
		if s[i] >= '0' && s[i] <= '9' {
			n = n*10 + int(s[i]-'0')
			has = true
		}
	}
	return out
}
