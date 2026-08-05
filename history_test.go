package main

import (
	"strings"
	"testing"
)

func feedString(h *History, s string) { h.Feed([]byte(s)) }

func TestHistoryPlainLines(t *testing.T) {
	h := newHistory(100)
	feedString(h, "hello\r\nworld\r\n")
	if h.Total() != 2 {
		t.Fatalf("want 2 lines, got %d", h.Total())
	}
	if h.LineAt(0).s != "hello" || h.LineAt(1).s != "world" {
		t.Fatalf("got %q, %q", h.LineAt(0).s, h.LineAt(1).s)
	}
}

func TestHistoryColorCarriesAcrossLines(t *testing.T) {
	h := newHistory(100)
	feedString(h, "\x1b[31mred\r\nstill\x1b[0m\r\n")
	if !strings.Contains(h.LineAt(0).s, "\x1b[31m") {
		t.Fatalf("line 0 lost color: %q", h.LineAt(0).s)
	}
	// second line began while red was active, so it must reopen the color
	if !strings.HasPrefix(h.LineAt(1).s, "\x1b[0;31m") {
		t.Fatalf("line 1 does not reopen color: %q", h.LineAt(1).s)
	}
}

// Ink-style live area: apps like Claude Code repaint their last lines by
// erasing and moving the cursor up. History must keep only the final state.
func TestHistoryInkRepaint(t *testing.T) {
	h := newHistory(100)
	feedString(h, "static\r\nliveA-old\r\nliveB-old")
	// erase current line, up one, erase, then rewrite both live lines
	feedString(h, "\x1b[2K\x1b[1A\x1b[2K\rliveA-new\r\nliveB-new")
	if h.Total() != 2 {
		t.Fatalf("want 2 completed lines, got %d", h.Total())
	}
	if h.LineAt(0).s != "static" || h.LineAt(1).s != "liveA-new" {
		t.Fatalf("got %q, %q", h.LineAt(0).s, h.LineAt(1).s)
	}
	if got := h.DocLine(2).s; got != "liveB-new" {
		t.Fatalf("in-progress line: %q", got)
	}
}

// Truecolor SGR state crossing many lines must reopen as one well-formed
// sequence, never as sliced fragments like ";215;119;87m" leaking into text.
func TestHistoryTruecolorNoFragments(t *testing.T) {
	h := newHistory(1000)
	for i := 0; i < 50; i++ {
		feedString(h, "\x1b[38;2;215;119;87m\x1b[48;2;30;30;30m\x1b[1mx\r\n")
	}
	feedString(h, "tail\r\n")
	last := h.LineAt(h.Total() - 1)
	if !strings.HasPrefix(last.s, "\x1b[0;1;38;2;215;119;87;48;2;30;30;30m") {
		t.Fatalf("bad canonical prefix: %q", last.s)
	}
	for i := 0; i < h.Total(); i++ {
		s := h.LineAt(i).s
		if len(s) > 0 && (s[0] == ';' || (s[0] >= '0' && s[0] <= '9')) {
			t.Fatalf("line %d starts with SGR fragment garbage: %q", i, s)
		}
	}
}

func TestHistoryCursorForwardIsSpacing(t *testing.T) {
	h := newHistory(10)
	feedString(h, "a\x1b[3Cb\r\n")
	if got := h.LineAt(0); got.s != "a   b" || got.w != 5 {
		t.Fatalf("got %q w=%d", got.s, got.w)
	}
	// column-absolute padding
	feedString(h, "x\x1b[5Gy\r\n")
	if got := h.LineAt(1); got.s != "x   y" || got.w != 5 {
		t.Fatalf("got %q w=%d", got.s, got.w)
	}
}

func TestHistoryCRRewrite(t *testing.T) {
	h := newHistory(100)
	feedString(h, "progress 10%\rprogress 99%\r\n")
	if h.LineAt(0).s != "progress 99%" {
		t.Fatalf("got %q", h.LineAt(0).s)
	}
}

func TestHistoryAltScreenSkipped(t *testing.T) {
	h := newHistory(100)
	feedString(h, "before\r\n\x1b[?1049hvim noise\x1b[?1049lafter\r\n")
	if h.Total() != 2 {
		t.Fatalf("want 2 lines, got %d", h.Total())
	}
	if h.LineAt(1).s != "after" {
		t.Fatalf("got %q", h.LineAt(1).s)
	}
}

func TestHistoryModeSniffing(t *testing.T) {
	h := newHistory(10)
	feedString(h, "\x1b[?1000h\x1b[?2004h\x1b[?1h")
	if !h.MouseOn || !h.Paste || !h.CursorKeys {
		t.Fatalf("flags not set: mouse=%v paste=%v cursorkeys=%v", h.MouseOn, h.Paste, h.CursorKeys)
	}
	feedString(h, "\x1b[?1000l")
	if h.MouseOn {
		t.Fatal("mouse flag not cleared")
	}
}

func TestHistoryTrimKeepsAbsoluteIndices(t *testing.T) {
	h := newHistory(10)
	for i := 0; i < 25; i++ {
		feedString(h, "line\r\n")
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
