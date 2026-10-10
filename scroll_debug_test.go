package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MurkyPuma/perch/internal/vt10x"
)

// TestScrollViewRealistic drives a zsh session that mimics a default macOS
// prompt, scrolls, and logs the reconstructed screens so the scroll view can
// be eyeballed against the live view. Guards the basics; the t.Log output is
// the real product.
func TestScrollViewRealistic(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "perch")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	zdot := filepath.Join(tmp, "zdot")
	os.MkdirAll(zdot, 0o755)
	os.WriteFile(filepath.Join(zdot, ".zshrc"), []byte("PROMPT='%n@%m %1~ %% '\n"), 0o644)

	env := []string{"PERCH_DIR=" + filepath.Join(tmp, "state"), "SHELL=/bin/zsh",
		"ZDOTDIR=" + zdot, "PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"TERM=xterm-256color"}
	defer func() {
		kill := exec.Command(bin, "kill-server")
		kill.Env = env
		kill.Run()
	}()

	tt := startClient(t, bin, env, "demo")

	m := tt.mark()
	tt.send("echo marker-one && echo marker-two\r")
	tt.waitFor(m, "marker-two")
	m = tt.mark()
	tt.send("printf 'no-trailing-newline'\r")
	tt.waitFor(m, "no-trailing-newline")
	m = tt.mark()
	tt.send("seq 1 40\r")
	tt.waitFor(m, "\x1b[0m40 ") // a painted row; bare "40" matches the typed command

	screen := func() string {
		tt.mu.Lock()
		defer tt.mu.Unlock()
		vt := vt10x.New(vt10x.WithSize(100, 28))
		vt.Write(tt.buf)
		return vt.String()
	}
	t.Logf("LIVE:\n%s", screen())

	m = tt.mark()
	tt.send("\x1b[<64;10;10M")
	tt.waitFor(m, "SCROLL")
	time.Sleep(200 * time.Millisecond)
	scroll1 := screen()
	t.Logf("SCROLL after 1 wheel:\n%s", scroll1)

	tt.send("\x1b[<64;10;10M\x1b[<64;10;10M\x1b[<64;10;10M")
	time.Sleep(300 * time.Millisecond)
	t.Logf("SCROLL after 4 wheels:\n%s", screen())

	m = tt.mark()
	tt.send("q")
	tt.waitFor(m, "demo")
	t.Logf("LIVE after q:\n%s", screen())

	if !strings.Contains(scroll1, "SCROLL") {
		t.Fatal("scroll indicator missing")
	}
}
