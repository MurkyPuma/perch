package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestHistoryWithRealZsh feeds actual zsh PTY output through the history
// parser, no vmux in the loop, to verify lines are captured.
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

	time.Sleep(1500 * time.Millisecond)
	ptmx.Write([]byte("seq 1 40\r"))
	time.Sleep(1200 * time.Millisecond)
	ptmx.Write([]byte("exit\r"))
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	raw := append([]byte{}, buf...)
	mu.Unlock()

	h := newHistory(1000)
	h.Feed(raw)
	t.Logf("raw bytes: %d, history lines: %d, altScreen=%v paste=%v",
		len(raw), h.Total(), h.AltScreen, h.Paste)
	if h.Total() < 40 {
		head := raw
		if len(head) > 1500 {
			head = head[:1500]
		}
		t.Fatalf("expected >=40 lines, got %d; first raw bytes:\n%q", h.Total(), head)
	}
}
