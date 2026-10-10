package main

import "github.com/MurkyPuma/perch/internal/vt10x"

// History is a session's scrollback: the lines that scrolled off the top of
// the primary screen, handed over by the terminal emulator as they leave. The
// emulator has already applied every cursor move, repaint and erase, so what
// lands here is exactly what was on screen, including output from apps like
// Claude Code that redraw in place. Alt-screen apps (vim, less) never push
// lines here.

type histLine struct {
	s string // line content with embedded SGR sequences
	w int    // printable width in cells
}

type History struct {
	lines   []histLine
	max     int
	trimmed int // count of lines dropped from the front; keeps indices absolute
}

func newHistory(max int) *History {
	return &History{max: max}
}

// Absolute line indices: stable as the ring trims, so a scroll anchor does not
// jump when old lines are dropped.
func (h *History) Total() int              { return h.trimmed + len(h.lines) }
func (h *History) First() int              { return h.trimmed }
func (h *History) LineAt(abs int) histLine { return h.lines[abs-h.trimmed] }

func (h *History) push(l histLine) {
	h.lines = append(h.lines, l)
	if len(h.lines) > h.max {
		drop := len(h.lines) - h.max
		h.lines = h.lines[drop:]
		h.trimmed += drop
	}
}

// pushGlyphs stores one emulator row, minus its trailing blank cells.
func (h *History) pushGlyphs(cells []vt10x.Glyph) {
	n := len(cells)
	for n > 0 && blankCell(cells[n-1]) {
		n--
	}
	h.push(histLine{s: glyphsLine(cells[:n]), w: n})
}

func blankCell(g vt10x.Glyph) bool {
	return (g.Char == ' ' || g.Char == 0) && g.BG == vt10x.DefaultBG
}
