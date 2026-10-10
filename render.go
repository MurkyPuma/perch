package main

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/MurkyPuma/perch/internal/vt10x"
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
	cells := make([]vt10x.Glyph, cols)
	for x := range cells {
		cells[x] = t.Cell(x, y)
	}
	return glyphsLine(cells)
}

// glyphsLine renders a row of cells into an ANSI string, one cell per column.
func glyphsLine(cells []vt10x.Glyph) string {
	var b strings.Builder
	lastFG := vt10x.DefaultFG
	lastBG := vt10x.DefaultBG
	for _, g := range cells {
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
	if lastFG != vt10x.DefaultFG || lastBG != vt10x.DefaultBG {
		b.WriteString("\x1b[0m")
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

// splitSpan divides total cells among n tiles with 1-cell separators.
func splitSpan(total, n int) []int {
	if n < 1 {
		n = 1
	}
	avail := total - (n - 1)
	base := avail / n
	rem := avail % n
	w := make([]int, n)
	for i := range w {
		w[i] = base
		if i < rem {
			w[i]++
		}
	}
	return w
}

// A rect is one tile's area on screen, 0-based, separators excluded.
type rect struct{ x, y, w, h int }

// tileRects lays n tiles out over the grid above the status bar: side by
// side in columns, or stacked top to bottom.
func tileRects(cols, rows, n int, stacked bool) []rect {
	var out []rect
	off := 0
	if stacked {
		for _, h := range splitSpan(rows-1, n) {
			out = append(out, rect{0, off, cols, h})
			off += h + 1
		}
		return out
	}
	for _, w := range splitSpan(cols, n) {
		out = append(out, rect{off, 0, w, rows - 1})
		off += w + 1
	}
	return out
}

// tileAt maps a 1-based screen point to (tile index, tile-local x, y), also
// 1-based. Returns -1 on separators and the status bar.
func tileAt(rects []rect, x, y int) (int, int, int) {
	for i, r := range rects {
		if x > r.x && x <= r.x+r.w && y > r.y && y <= r.y+r.h {
			return i, x - r.x, y - r.y
		}
	}
	return -1, x, y
}

// rects lays out the client's current view. Caller must hold c.mu.
func (c *client) rects() []rect {
	return tileRects(c.cols, c.rows, len(c.view), c.stacked)
}

// statusBar builds the bottom row. The focused session gets its identity color
// as a bright segment; other visible tiles show their name in their identity
// color; background sessions are dim. Caller must hold srv.mu.
func statusBar(srv *Server, view []*Session, focus int, cols int, extra string) (string, []clickRange) {
	cur := view[focus]
	visible := map[*Session]bool{}
	for _, v := range view {
		visible[v] = true
	}
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
		switch {
		case s == cur:
			line.add(fmt.Sprintf("0;1;38;5;16;48;5;%d", identityColor(s.name)), seg)
		case visible[s]:
			line.add(fmt.Sprintf("0;48;5;236;38;5;%d", identityColor(s.name)), seg)
		default:
			line.add(styleBarDim, seg)
		}
		ranges = append(ranges, clickRange{x0: start, x1: line.w, name: s.name})
	}
	right := extra
	style := styleBarDim
	if extra == "" {
		// contextual shortcuts; drop from the left when the window is narrow
		segs := []string{"wheel scroll", "^Space sessions", "^\\ detach"}
		if len(view) > 1 {
			segs = append([]string{"^O focus"}, segs...)
		}
		for len(segs) > 0 {
			right = strings.Join(segs, " · ") + " "
			if line.w+runeWidth(right) <= cols {
				break
			}
			segs = segs[1:]
			right = ""
		}
	} else {
		style = styleScrollBar
	}
	gap := cols - line.w - runeWidth(right)
	line.pad(styleBarBG, gap)
	line.add(style, right)
	return line.String(), ranges
}

// renderLive paints the visible tiles side by side plus the status bar.
// Caller must hold c.mu.
func (c *client) renderLive() *frame {
	return c.renderTiles(false)
}

// renderScroll is the live layout with the focused tile showing its
// scrollback, so in a split the other tiles keep running beside it.
// Caller must hold c.mu.
func (c *client) renderScroll() *frame {
	return c.renderTiles(true)
}

func (c *client) renderTiles(scroll bool) *frame {
	view := c.view
	rects := c.rects()
	f := &frame{lines: make([]string, 0, c.rows)}

	tiles := make([][]string, len(view))
	extra := ""
	for i, s := range view {
		r := rects[i]
		if scroll && i == c.focus {
			tiles[i], extra = c.scrollTile(s, r.w, r.h)
			continue
		}
		lines := make([]string, r.h)
		s.mu.Lock()
		vcols, vrows := s.vt.Size()
		s.vt.Lock()
		cw := min(vcols, r.w)
		for y := 0; y < r.h; y++ {
			var ln string
			if y < vrows {
				ln = gridLine(s.vt, y, cw)
			} else {
				cw = 0
			}
			if cw < r.w {
				ln += "\x1b[0m" + strings.Repeat(" ", r.w-cw)
			}
			lines[y] = ln
		}
		if i == c.focus {
			cur := s.vt.Cursor()
			f.curX, f.curY = r.x+cur.X, r.y+cur.Y
			f.curVis = s.vt.CursorVisible() && cur.Y < r.h && cur.X < r.w
		}
		s.vt.Unlock()
		s.mu.Unlock()
		tiles[i] = lines
	}

	switch {
	case len(view) == 1:
		f.lines = append(f.lines, tiles[0]...)
	case c.stacked:
		hsep := "\x1b[0;38;5;238m" + strings.Repeat("─", c.cols) + "\x1b[0m"
		for i := range view {
			if i > 0 {
				f.lines = append(f.lines, hsep)
			}
			f.lines = append(f.lines, tiles[i]...)
		}
	default:
		const vsep = "\x1b[0;38;5;238m│\x1b[0m"
		parts := make([]string, len(view))
		for y := 0; y < c.rows-1; y++ {
			for i := range view {
				parts[i] = tiles[i][y]
			}
			f.lines = append(f.lines, strings.Join(parts, vsep))
		}
	}

	c.srv.mu.Lock()
	bar, ranges := statusBar(c.srv, view, c.focus, c.cols, extra)
	c.srv.mu.Unlock()
	c.statusRanges = ranges
	f.lines = append(f.lines, bar)
	return f
}

// scrollTile renders the scrollback document anchored at c.anchor (an
// absolute line index) into a tile w cells wide, padding every row to w so
// neighbouring tiles stay aligned. Returns the rows and the status text.
// Caller must hold c.mu.
func (c *client) scrollTile(s *Session, w, rows int) ([]string, string) {
	lines := make([]string, 0, rows)
	add := func(ln string, lw int) {
		if lw < w {
			ln += "\x1b[0m" + strings.Repeat(" ", w-lw)
		}
		lines = append(lines, ln)
	}

	s.mu.Lock()
	s.vt.Lock()
	total := s.docLen()
	first := s.hist.First()
	if c.anchor < first {
		c.anchor = first
	}
	pos := c.anchor - first + 1
	for abs := c.anchor; len(lines) < rows && abs < total; abs++ {
		hl := s.docLine(abs)
		if hl.w <= w {
			add(hl.s, hl.w)
			continue
		}
		chunks := wrapLine(hl.s, w)
		for k, chunk := range chunks {
			if len(lines) >= rows {
				break
			}
			cw := w
			if k == len(chunks)-1 {
				cw = hl.w - k*w
			}
			add(chunk, cw)
		}
	}
	s.vt.Unlock()
	s.mu.Unlock()
	for len(lines) < rows {
		add("\x1b[38;5;240m~", 1)
	}
	return lines, fmt.Sprintf(" SCROLL %d/%d · wheel/PgUp/PgDn · q quit ", pos, total-first)
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
	title.add("0;1;38;5;252", "  perch sessions")
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
		} else {
			for _, v := range c.view {
				if v == s {
					notes = append(notes, "in split")
				}
			}
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
		row = 6
	}

	// visible legend: the split action lives here and people must find it
	if row+2 < c.rows-2 {
		var l sb
		l.add("0;38;5;250", "  ↵")
		l.add("0;38;5;244", " open here      ")
		l.add("0;38;5;250", "^S")
		l.add("0;38;5;244", " open side-by-side      ")
		l.add("0;38;5;250", "^L")
		l.add("0;38;5;244", " flip layout      ")
		l.add("0;38;5;250", "^X")
		l.add("0;38;5;244", " kill")
		f.lines[row+2] = l.String()
	}

	var hints sb
	hints.add(styleBarBG, " ")
	hints.add(styleBarDim, "↵ switch/create · ^S split · ^L flip · 1-9 jump · ^X kill · ^D detach · esc close")
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
