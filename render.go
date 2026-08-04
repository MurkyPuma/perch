package main

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/hinshun/vt10x"
)

// A frame is a full screen to present: one string per row (self-contained SGR,
// assumes default state at start of row) plus cursor placement.
type frame struct {
	lines  []string
	curX   int // 0-based
	curY   int
	curVis bool
}

// Identity colors: each session gets a stable 256-color pair derived from its
// name, used in the status bar and palette so a session is recognizable at a
// glance.
var identityBG = []int{114, 111, 221, 176, 80, 210, 216, 147, 150, 117}

func identityColor(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return identityBG[int(h.Sum32())%len(identityBG)]
}

// sb builds a styled status/palette line while tracking printable width.
type sb struct {
	b strings.Builder
	w int
}

func (s *sb) add(style, text string) {
	if style != "" {
		s.b.WriteString("\x1b[" + style + "m")
	}
	s.b.WriteString(text)
	s.w += runeWidth(text)
}

func (s *sb) pad(style string, n int) {
	if n <= 0 {
		return
	}
	s.add(style, strings.Repeat(" ", n))
}

func (s *sb) String() string { return s.b.String() }

func runeWidth(text string) int {
	n := 0
	for range text {
		n++
	}
	return n
}

// sgrForColor appends the SGR params selecting color c as fg or bg.
func sgrForColor(c vt10x.Color, fg bool, parts *[]string) {
	switch {
	case c == vt10x.DefaultFG:
		*parts = append(*parts, "39")
	case c == vt10x.DefaultBG:
		*parts = append(*parts, "49")
	case c < 8:
		if fg {
			*parts = append(*parts, fmt.Sprintf("%d", 30+int(c)))
		} else {
			*parts = append(*parts, fmt.Sprintf("%d", 40+int(c)))
		}
	case c < 16:
		if fg {
			*parts = append(*parts, fmt.Sprintf("%d", 90+int(c)-8))
		} else {
			*parts = append(*parts, fmt.Sprintf("%d", 100+int(c)-8))
		}
	case c < 256:
		if fg {
			*parts = append(*parts, fmt.Sprintf("38;5;%d", int(c)))
		} else {
			*parts = append(*parts, fmt.Sprintf("48;5;%d", int(c)))
		}
	case c >= 1<<24: // DefaultFG/BG/Cursor already handled; be safe
		if fg {
			*parts = append(*parts, "39")
		} else {
			*parts = append(*parts, "49")
		}
	default: // truecolor packed r<<16|g<<8|b
		r, g, b := int(c>>16)&0xff, int(c>>8)&0xff, int(c)&0xff
		if fg {
			*parts = append(*parts, fmt.Sprintf("38;2;%d;%d;%d", r, g, b))
		} else {
			*parts = append(*parts, fmt.Sprintf("48;2;%d;%d;%d", r, g, b))
		}
	}
}

// gridLine renders row y of the virtual terminal into an ANSI string.
// Caller must hold the session lock.
func gridLine(t vt10x.Terminal, y, cols int) string {
	var b strings.Builder
	lastFG := vt10x.DefaultFG
	lastBG := vt10x.DefaultBG
	for x := 0; x < cols; x++ {
		g := t.Cell(x, y)
		if g.FG != lastFG || g.BG != lastBG {
			var parts []string
			sgrForColor(g.FG, true, &parts)
			sgrForColor(g.BG, false, &parts)
			b.WriteString("\x1b[" + strings.Join(parts, ";") + "m")
			lastFG, lastBG = g.FG, g.BG
		}
		ch := g.Char
		if ch == 0 {
			ch = ' '
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// wrapLine splits a string with embedded SGR sequences into chunks of at most
// width printable cells, carrying color state into continuation chunks.
func wrapLine(s string, width int) []string {
	if width < 1 {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	var sgr string
	w := 0
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
					j++
				}
				if j < len(s) {
					j++
				}
				seq := s[i:j]
				cur.WriteString(seq)
				if strings.HasSuffix(seq, "m") {
					if seq == "\x1b[m" || seq == "\x1b[0m" {
						sgr = ""
					} else {
						sgr += seq
					}
				}
				i = j
				continue
			}
			i++
			continue
		}
		// one rune
		j := i + 1
		for j < len(s) && s[j]&0xc0 == 0x80 {
			j++
		}
		if w == width {
			out = append(out, cur.String())
			cur.Reset()
			cur.WriteString(sgr)
			w = 0
		}
		cur.WriteString(s[i:j])
		w++
		i = j
	}
	out = append(out, cur.String())
	return out
}

const (
	styleBarBG     = "0;48;5;236;38;5;250"
	styleBarDim    = "0;48;5;236;38;5;244"
	styleScrollBar = "0;48;5;58;38;5;227;1"
)

type clickRange struct {
	x0, x1 int // 1-based inclusive columns on the status row
	name   string
}

// statusBar builds the bottom row. Caller must hold srv.mu.
func statusBar(srv *Server, cur *Session, cols int, extra string) (string, []clickRange) {
	var line sb
	var ranges []clickRange
	line.add(styleBarBG, " ")
	for _, s := range srv.sessionsByName() {
		seg := " " + s.name + " "
		start := line.w + 1
		if line.w+len(seg)+2 > cols-2 {
			line.add(styleBarDim, "…")
			break
		}
		if s == cur {
			bg := identityColor(s.name)
			line.add(fmt.Sprintf("0;1;38;5;16;48;5;%d", bg), seg)
		} else {
			line.add(styleBarDim, seg)
		}
		ranges = append(ranges, clickRange{x0: start, x1: line.w, name: s.name})
	}
	right := extra
	if right == "" {
		right = "^Space sessions · ^\\ detach "
	}
	style := styleBarDim
	if extra != "" {
		style = styleScrollBar
	}
	gap := cols - line.w - runeWidth(right)
	line.pad(styleBarBG, gap)
	line.add(style, right)
	return line.String(), ranges
}

// renderLive paints the active session's screen plus the status bar.
// Caller must hold c.mu.
func (c *client) renderLive() *frame {
	s := c.sess
	f := &frame{lines: make([]string, 0, c.rows)}
	gridRows := c.rows - 1

	s.mu.Lock()
	vcols, vrows := s.vt.Size()
	s.vt.Lock()
	for y := 0; y < gridRows; y++ {
		if y < vrows {
			f.lines = append(f.lines, gridLine(s.vt, y, min(vcols, c.cols)))
		} else {
			f.lines = append(f.lines, "")
		}
	}
	cur := s.vt.Cursor()
	f.curX, f.curY = cur.X, cur.Y
	f.curVis = s.vt.CursorVisible() && cur.Y < gridRows
	s.vt.Unlock()
	s.mu.Unlock()

	c.srv.mu.Lock()
	bar, ranges := statusBar(c.srv, s, c.cols, "")
	c.srv.mu.Unlock()
	c.statusRanges = ranges
	f.lines = append(f.lines, bar)
	return f
}

// renderScroll paints the scrollback view anchored at c.anchor (an absolute
// logical line index into the session history). Caller must hold c.mu.
func (c *client) renderScroll() *frame {
	s := c.sess
	f := &frame{lines: make([]string, 0, c.rows)}
	viewRows := c.rows - 1

	s.mu.Lock()
	total := s.hist.Total()
	first := s.hist.First()
	if c.anchor < first {
		c.anchor = first
	}
	pos := 0
	if total > 0 {
		pos = c.anchor - first + 1
	}
	for abs := c.anchor; len(f.lines) < viewRows && abs < total; abs++ {
		hl := s.hist.LineAt(abs)
		if hl.w <= c.cols {
			f.lines = append(f.lines, hl.s)
			continue
		}
		for _, chunk := range wrapLine(hl.s, c.cols) {
			if len(f.lines) >= viewRows {
				break
			}
			f.lines = append(f.lines, chunk)
		}
	}
	s.mu.Unlock()
	for len(f.lines) < viewRows {
		f.lines = append(f.lines, "\x1b[38;5;240m~")
	}

	extra := fmt.Sprintf(" SCROLL %d/%d · wheel/PgUp/PgDn · q quit ", pos, total-first)
	c.srv.mu.Lock()
	bar, ranges := statusBar(c.srv, s, c.cols, extra)
	c.srv.mu.Unlock()
	c.statusRanges = ranges
	f.lines = append(f.lines, bar)
	f.curVis = false
	return f
}

// renderPalette paints the full-screen session switcher. Caller must hold c.mu.
type palRow struct {
	y    int // 1-based screen row
	name string
}

func (c *client) renderPalette() *frame {
	f := &frame{lines: make([]string, c.rows)}
	list := c.filteredSessions()
	c.palRows = c.palRows[:0]
	if c.sel >= len(list) {
		c.sel = len(list) - 1
	}
	if c.sel < 0 {
		c.sel = 0
	}

	var title sb
	title.add("0;1;38;5;252", "  vmux sessions")
	f.lines[1] = title.String()

	var input sb
	input.add("0;38;5;245", "  › ")
	input.add("0;38;5;255", string(c.filter))
	f.lines[3] = input.String()
	f.curX, f.curY, f.curVis = input.w, 3, true

	row := 5
	for i, s := range list {
		if row >= c.rows-2 {
			break
		}
		var l sb
		style := "0;38;5;250"
		dotStyle := fmt.Sprintf("0;38;5;%d", identityColor(s.name))
		if i == c.sel {
			style = "0;1;38;5;16;48;5;252"
			dotStyle = fmt.Sprintf("0;48;5;252;38;5;%d", identityColor(s.name))
		}
		l.add(style, fmt.Sprintf("  %d ", i+1))
		l.add(dotStyle, "●")
		l.add(style, " "+s.name+" ")
		var notes []string
		if s == c.sess {
			notes = append(notes, "current")
		}
		if n := c.srv.attachedCount(s); n > 0 {
			notes = append(notes, fmt.Sprintf("%d attached", n))
		}
		if len(notes) > 0 {
			noteStyle := "0;38;5;244"
			if i == c.sel {
				noteStyle = "0;38;5;240;48;5;252"
			}
			l.add(noteStyle, "· "+strings.Join(notes, " · ")+" ")
		}
		f.lines[row] = l.String()
		c.palRows = append(c.palRows, palRow{y: row + 1, name: s.name})
		row++
	}
	if len(list) == 0 && len(c.filter) > 0 {
		var l sb
		l.add("0;38;5;114", fmt.Sprintf("  ↵ create session %q", sanitizeName(string(c.filter))))
		f.lines[5] = l.String()
	}

	var hints sb
	hints.add(styleBarBG, " ")
	hints.add(styleBarDim, "↵ switch/create · type to filter · 1-9 jump · ^X kill · ^D detach · esc close")
	hints.pad(styleBarBG, c.cols-hints.w)
	f.lines[c.rows-1] = hints.String()
	c.statusRanges = nil
	return f
}

// paint diffs the frame against the previous one and sends the delta.
// Caller must hold c.mu.
func (c *client) paint(f *frame) {
	var b strings.Builder
	b.WriteString("\x1b[?2026h\x1b[?25l")
	full := c.prev == nil || len(c.prev) != len(f.lines)
	if full {
		b.WriteString("\x1b[0m\x1b[2J")
	}
	for i, line := range f.lines {
		if !full && c.prev[i] == line {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[0m", i+1)
		b.WriteString(line)
		b.WriteString("\x1b[0m\x1b[K")
	}
	if f.curVis {
		fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", f.curY+1, f.curX+1)
	}
	b.WriteString("\x1b[?2026l")
	c.prev = f.lines
	c.mc.writeMsg(msgOutput, []byte(b.String()))
}
