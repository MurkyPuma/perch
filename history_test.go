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
	if !strings.HasPrefix(h.LineAt(1).s, "\x1b[31m") {
		t.Fatalf("line 1 does not reopen color: %q", h.LineAt(1).s)
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
