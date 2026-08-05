package main

import (
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

// A Session is one shell on one PTY, plus a virtual terminal that tracks its
// screen (so we can repaint it on switch/attach) and a history buffer for
// scrollback. Sessions live in the server and survive client detach.
type Session struct {
	name string

	mu   sync.Mutex
	cmd  *exec.Cmd
	ptmx *os.File
	vt   vt10x.Terminal
	hist *History

	cols, rows int
	lastActive time.Time
	dead       bool
}

const historyMax = 10000

func newSession(name, cwd string, cols, rows int) (*Session, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "PERCH="+name)
	if cwd != "" {
		if st, err := os.Stat(cwd); err == nil && st.IsDir() {
			cmd.Dir = cwd
		}
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	s := &Session{
		name:       name,
		cmd:        cmd,
		ptmx:       ptmx,
		vt:         vt10x.New(vt10x.WithSize(cols, rows)),
		hist:       newHistory(historyMax),
		cols:       cols,
		rows:       rows,
		lastActive: time.Now(),
	}
	return s, nil
}

// readLoop pumps PTY output into the virtual terminal and history, then asks
// the server to repaint any clients viewing this session. Runs until the
// shell exits.
func (s *Session) readLoop(srv *Server) {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.vt.Write(buf[:n])
			s.hist.Feed(buf[:n])
			s.mu.Unlock()
			srv.sessionUpdated(s)
		}
		if err != nil {
			break
		}
	}
	s.cmd.Wait()
	srv.sessionClosed(s)
}

func (s *Session) resize(cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead || (cols == s.cols && rows == s.rows) {
		return
	}
	s.cols, s.rows = cols, rows
	pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	s.vt.Resize(cols, rows)
}

func (s *Session) altScreen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hist.AltScreen
}

func (s *Session) mouseOn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hist.MouseOn
}

func (s *Session) writeInput(p []byte) {
	s.mu.Lock()
	ptmx, dead := s.ptmx, s.dead
	s.mu.Unlock()
	if dead {
		return
	}
	if _, err := ptmx.Write(p); err != nil {
		log.Printf("session %s: input write: %v", s.name, err)
	}
}

// kill terminates the session's process group and closes the PTY. The
// readLoop notices and runs the server-side cleanup.
func (s *Session) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	if s.cmd.Process != nil {
		s.cmd.Process.Signal(os.Interrupt)
		s.cmd.Process.Kill()
	}
	s.ptmx.Close()
}
