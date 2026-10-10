package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MurkyPuma/perch/internal/vt10x"
	"github.com/creack/pty"
)

// TestHistoryWithRealZsh feeds actual zsh PTY output through the history
// capture, no perch in the loop, to verify lines are captured.
func TestHistoryWithRealZsh(t *testing.T) {
	tmp := t.TempDir()
	zdot := filepath.Join(tmp, "zdot")
	os.MkdirAll(zdot, 0o755)
	os.WriteFile(filepath.Join(zdot, ".zshrc"), []byte("PROMPT='%n@%m %1~ %% '\n"), 0o644)

	cmd := exec.Command("/bin/zsh", "-l")
	cmd.Env = append(os.Environ(), "ZDOTDIR="+zdot, "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 28})
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()

	var mu sync.Mutex
	var buf []byte
	go func() {
		b := make([]byte, 8192)
		for {
			n, err := ptmx.Read(b)
			if n > 0 {
				mu.Lock()
				buf = append(buf, b[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	waitFor := func(substr string) {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
			mu.Lock()
			ok := strings.Contains(string(buf), substr)
			mu.Unlock()
			if ok {
				return
			}
		}
		t.Fatalf("timeout waiting for %q", substr)
	}
	waitFor("% ")
	ptmx.Write([]byte("seq 1 40\r"))
	waitFor("\n40\r\n")

	mu.Lock()
	raw := append([]byte{}, buf...)
	mu.Unlock()

	h := newHistory(1000)
	vt := vt10x.New(vt10x.WithSize(100, 28), vt10x.WithScrollback(h.pushGlyphs))
	vt.Write(raw)
	t.Logf("raw bytes: %d, history lines: %d", len(raw), h.Total())
	// every line of seq output is in history or still on screen, once
	doc := strings.Join(histStrings(h), "\n") + "\n" + vt.String()
	lines := strings.Split(doc, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	doc = strings.Join(lines, "\n")
	for n := 1; n <= 40; n++ {
		if c := strings.Count("\n"+doc+"\n", fmt.Sprintf("\n%d\n", n)); c != 1 {
			t.Fatalf("line %d appears %d times; history:\n%s", n, c, strings.Join(histStrings(h), "\n"))
		}
	}
}
