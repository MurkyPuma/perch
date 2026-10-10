package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MurkyPuma/perch/internal/vt10x"
)

// feedTerm runs s through a cols x rows emulator wired to a fresh history,
// the same way a session does.
func feedTerm(cols, rows int, s string) (*History, vt10x.Terminal) {
	h := newHistory(1000)
	vt := vt10x.New(vt10x.WithSize(cols, rows), vt10x.WithScrollback(h.pushGlyphs))
	vt.Write([]byte(s))
	return h, vt
}

func histStrings(h *History) []string {
	var out []string
	for i := h.First(); i < h.Total(); i++ {
		out = append(out, h.LineAt(i).s)
	}
	return out
}

func TestScrollbackPlainLines(t *testing.T) {
	h, _ := feedTerm(10, 3, "a\r\nb\r\nc\r\nd\r\n")
	if got := histStrings(h); strings.Join(got, ",") != "a,b" {
		t.Fatalf("got %q", got)
	}
	if h.LineAt(0).w != 1 {
		t.Fatalf("trailing blanks not trimmed: w=%d", h.LineAt(0).w)
	}
}

func TestScrollbackKeepsColor(t *testing.T) {
	h, _ := feedTerm(10, 2, "\x1b[31mred\x1b[0m\r\nx\r\ny\r\n")
	if got := h.LineAt(0).s; got != "\x1b[31;49mred\x1b[0m" {
		t.Fatalf("got %q", got)
	}
}

// Claude Code / Ink repaint their live area in place: cursor up, rewrite a
// line, cursor back down. Only what actually scrolls off may land in history,
// in its final state, and nothing may be lost.
func TestScrollbackInkRepaint(t *testing.T) {
	var b strings.Builder
	b.WriteString("one\r\ntwo\r\nspin-a\r\nstatus")
	b.WriteString("\x1b[1A\r\x1b[2Kspin-b\x1b[1B\r") // repaint spinner, go back down
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&b, "\r\nout-%d", i)
	}
	h, _ := feedTerm(20, 4, b.String())
	want := "one,two,spin-b,status,out-1,out-2"
	if got := strings.Join(histStrings(h), ","); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestScrollbackAltScreenSkipped(t *testing.T) {
	h, _ := feedTerm(20, 2, "before\r\n\x1b[?1049h"+strings.Repeat("vim noise\r\n", 5)+"\x1b[?1049l\r\nafter\r\n")
	for _, l := range histStrings(h) {
		if strings.Contains(l, "vim") {
			t.Fatalf("alt screen leaked into history: %q", histStrings(h))
		}
	}
}

func TestScrollbackModes(t *testing.T) {
	_, vt := feedTerm(10, 2, "\x1b[?2004h\x1b[?1000h")
	if vt.Mode()&vt10x.ModePaste == 0 || vt.Mode()&vt10x.ModeMouseMask == 0 {
		t.Fatalf("modes not tracked: %b", vt.Mode())
	}
}

// A multibyte rune split across two PTY reads must not be dropped.
func TestWriteSplitRune(t *testing.T) {
	vt := vt10x.New(vt10x.WithSize(10, 2))
	p := []byte("a─b")
	n, _ := vt.Write(p[:3]) // "a" plus 2 of the 3 bytes of "─"
	if n != 1 {
		t.Fatalf("consumed %d, want 1", n)
	}
	vt.Write(append(p[n:3:3], p[3:]...))
	if got := strings.TrimSpace(strings.Split(vt.String(), "\n")[0]); got != "a─b" {
		t.Fatalf("got %q", got)
	}
}

func TestHistoryTrimKeepsAbsoluteIndices(t *testing.T) {
	h := newHistory(10)
	for i := 0; i < 25; i++ {
		h.push(histLine{s: "line", w: 4})
	}
	if h.Total() != 25 || h.First() != 15 {
		t.Fatalf("total=%d first=%d", h.Total(), h.First())
	}
	if h.LineAt(24).s != "line" {
		t.Fatalf("got %q", h.LineAt(24).s)
	}
}

func TestParseMouseX10(t *testing.T) {
	// wheel up at (10, 5) in Terminal.app's legacy encoding
	me, n := parseMouse([]byte{0x1b, '[', 'M', 32 + 64, 32 + 10, 32 + 5})
	if n != 6 || me.btn != mouseWheelUp || me.x != 10 || me.y != 5 || !me.press {
		t.Fatalf("wheel: got %+v n=%d", me, n)
	}
	// left button press, then release
	me, n = parseMouse([]byte{0x1b, '[', 'M', 32 + 0, 33, 33})
	if n != 6 || me.btn != 0 || !me.press {
		t.Fatalf("press: got %+v n=%d", me, n)
	}
	me, n = parseMouse([]byte{0x1b, '[', 'M', 32 + 3, 33, 33})
	if n != 6 || me.press {
		t.Fatalf("release: got %+v n=%d", me, n)
	}
	// incomplete prefix asks for more bytes
	if _, n := parseMouse([]byte{0x1b, '[', 'M', 96}); n != 0 {
		t.Fatalf("incomplete: n=%d", n)
	}
	// SGR encoding still parses
	me, n = parseMouse([]byte("\x1b[<64;10;10M"))
	if n == 0 || me.btn != mouseWheelUp || me.x != 10 {
		t.Fatalf("sgr: got %+v n=%d", me, n)
	}
}

func TestWrapLinePreservesColor(t *testing.T) {
	chunks := wrapLine("\x1b[32maaaabbbb\x1b[0m", 4)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d: %q", len(chunks), chunks)
	}
	if !strings.HasPrefix(chunks[1], "\x1b[32m") {
		t.Fatalf("continuation lost color: %q", chunks[1])
	}
}

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"Soda Vault": "soda-vault",
		"  Weird!@#": "weird",
		"":           "main",
		"claimable":  "claimable",
		"a_b.c-d":    "a_b.c-d",
	} {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Claude Code probes for keyboard protocols mid-session with CSI ?u, CSI <u
// and CSI >4m. Read without their prefix these restore the cursor and reset
// colors, after which Ink's relative repaints land on the wrong rows.
func TestPrefixedQueriesIgnored(t *testing.T) {
	vt := vt10x.New(vt10x.WithSize(20, 4))
	vt.Write([]byte("\x1b7\x1b[3;5H\x1b[31m\x1b[?u\x1b[<u\x1b[>4m\x1b[>0qX"))
	if c := vt.Cursor(); c.X != 5 || c.Y != 2 {
		t.Fatalf("cursor moved to %d,%d", c.X, c.Y)
	}
	if fg := vt.Cell(4, 2).FG; fg != 1 {
		t.Fatalf("color reset: fg=%d", fg)
	}
}

// Text wrapped in a narrow split re-wraps when the session widens again, and
// rows that no longer fit when it narrows go to scrollback, not nowhere.
func TestResizeReflows(t *testing.T) {
	h, vt := feedTerm(10, 4, "abcdefghijklmno\r\nshort\r\n$ ")
	vt.Resize(20, 4)
	rows := strings.Split(vt.String(), "\n")
	if strings.TrimRight(rows[0], " ") != "abcdefghijklmno" || strings.TrimRight(rows[1], " ") != "short" {
		t.Fatalf("not reflowed on widen:\n%s", vt.String())
	}
	if c := vt.Cursor(); c.X != 2 || c.Y != 2 {
		t.Fatalf("cursor at %d,%d, want 2,2", c.X, c.Y)
	}

	vt.Resize(5, 4) // 3 + 1 + 1 rows: the first row must go to scrollback
	if got := strings.Join(histStrings(h), ","); got != "abcde" {
		t.Fatalf("history %q", got)
	}
	if c := vt.Cursor(); c.X != 2 || c.Y != 3 {
		t.Fatalf("cursor at %d,%d, want 2,3", c.X, c.Y)
	}
}
