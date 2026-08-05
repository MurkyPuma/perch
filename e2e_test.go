package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// term drives one perch client under a real PTY and accumulates its output.
type testTerm struct {
	t    *testing.T
	ptmx *os.File
	mu   sync.Mutex
	buf  []byte
}

func (tt *testTerm) readPump() {
	b := make([]byte, 4096)
	for {
		n, err := tt.ptmx.Read(b)
		if n > 0 {
			tt.mu.Lock()
			tt.buf = append(tt.buf, b[:n]...)
			tt.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// mark returns the current output offset; waitFor searches only after it.
func (tt *testTerm) mark() int {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return len(tt.buf)
}

func (tt *testTerm) waitFor(from int, substr string) {
	tt.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		tt.mu.Lock()
		got := string(tt.buf[min(from, len(tt.buf)):])
		tt.mu.Unlock()
		if strings.Contains(got, substr) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	tt.mu.Lock()
	tail := string(tt.buf[min(from, len(tt.buf)):])
	tt.mu.Unlock()
	if len(tail) > 2000 {
		tail = tail[len(tail)-2000:]
	}
	tt.t.Fatalf("timeout waiting for %q, output tail:\n%q", substr, tail)
}

func (tt *testTerm) send(s string) {
	tt.t.Helper()
	if _, err := tt.ptmx.Write([]byte(s)); err != nil {
		tt.t.Fatalf("send %q: %v", s, err)
	}
}

func startClient(t *testing.T, bin string, env []string, args ...string) *testTerm {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 28})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	tt := &testTerm{t: t, ptmx: ptmx}
	go tt.readPump()
	t.Cleanup(func() { ptmx.Close(); cmd.Wait() })
	// Wait for the first painted frame so tests don't type into the pty's
	// canonical-mode echo before the client has attached and gone raw.
	tt.waitFor(0, "\x1b[?2026l")
	return tt
}

func TestEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "perch")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	env := []string{"PERCH_DIR=" + filepath.Join(tmp, "state"), "SHELL=/bin/sh",
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TERM=xterm-256color"}
	defer exec.Command(bin, "kill-server").Run()
	defer func() {
		cmd := exec.Command(bin, "kill-server")
		cmd.Env = env
		cmd.Run()
	}()

	tt := startClient(t, bin, env, "alpha")

	// shell comes up, commands run
	m := tt.mark()
	tt.send("echo hello-from-alpha\r")
	tt.waitFor(m, "hello-from-alpha")

	// palette opens, typing an unknown name creates a session
	m = tt.mark()
	tt.send("\x00")
	tt.waitFor(m, "perch sessions")
	m = tt.mark()
	tt.send("work\r")
	tt.waitFor(m, " work ") // status bar segment for the new current session
	m = tt.mark()
	tt.send("echo hello-from-work\r")
	tt.waitFor(m, "hello-from-work")

	// double-tap Ctrl-Space jumps back to the previous session (alt-tab)
	m = tt.mark()
	tt.send("\x00\x00")
	tt.waitFor(m, "hello-from-alpha") // alpha's screen repaints

	// scrollback: generate history, wheel up, indicator appears
	m = tt.mark()
	tt.send("seq 1 200\r")
	tt.waitFor(m, "200")
	m = tt.mark()
	tt.send("\x1b[<64;10;10M") // SGR wheel up
	tt.waitFor(m, "SCROLL")
	m = tt.mark()
	tt.send("q")
	tt.waitFor(m, " alpha ") // live view again

	// perch ls sees both sessions
	lsCmd := exec.Command(bin, "ls")
	lsCmd.Env = env
	out, err := lsCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ls: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "alpha") || !strings.Contains(string(out), "work") {
		t.Fatalf("ls missing sessions:\n%s", out)
	}

	// detach; sessions survive; reattach sees the old screen
	m = tt.mark()
	tt.send("\x1c") // Ctrl-backslash
	tt.waitFor(m, "detached")

	// the reattached screen shows the tail of seq's output and the status bar
	tt2 := startClient(t, bin, env, "alpha")
	tt2.waitFor(0, "199")
	tt2.waitFor(0, " alpha ")

	// killing the server tears everything down
	killCmd := exec.Command(bin, "kill-server")
	killCmd.Env = env
	if out, err := killCmd.CombinedOutput(); err != nil {
		t.Fatalf("kill-server: %v\n%s", err, out)
	}
}

func TestSplitView(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "perch")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	env := []string{"PERCH_DIR=" + filepath.Join(tmp, "state"), "SHELL=/bin/sh",
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TERM=xterm-256color"}
	defer func() {
		kill := exec.Command(bin, "kill-server")
		kill.Env = env
		kill.Run()
	}()

	tt := startClient(t, bin, env, "left")

	// create a second session via the palette
	m := tt.mark()
	tt.send("\x00")
	tt.waitFor(m, "perch sessions")
	m = tt.mark()
	tt.send("right\r")
	tt.waitFor(m, " right ")
	m = tt.mark()
	tt.send("echo in-right\r")
	tt.waitFor(m, "in-right")

	// open palette, Ctrl-S the selected (previous) session into a split
	m = tt.mark()
	tt.send("\x00")
	tt.waitFor(m, "perch sessions")
	m = tt.mark()
	tt.send("\x13")    // Ctrl-S
	tt.waitFor(m, "│") // separator means two tiles are visible

	// focused tile is the newly added one (left); type into it
	m = tt.mark()
	tt.send("echo in-left\r")
	tt.waitFor(m, "in-left")

	// Ctrl-O cycles focus back to right; typing lands there
	tt.send("\x0f")
	m = tt.mark()
	tt.send("echo right-again\r")
	tt.waitFor(m, "right-again")

	// collapse back to a single session via palette + Enter
	m = tt.mark()
	tt.send("\x00")
	tt.waitFor(m, "perch sessions")
	tt.send("left\r")
	m = tt.mark()
	tt.send("echo solo\r")
	tt.waitFor(m, "solo")
}
