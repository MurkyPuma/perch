package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// The client is a dumb pipe: it puts the terminal in raw mode, forwards stdin
// and resizes to the server, and writes whatever the server sends to stdout.
// All key handling, rendering, and session logic lives server-side, which is
// what makes detach/reattach and multi-client attach trivial.

// sessionNameFor derives a session name from the working directory, so bare
// `perch` inside a project folder gives you a session named after the project.
func sessionNameFor(cwd string) string {
	home, _ := os.UserHomeDir()
	if cwd == "" || cwd == "/" || cwd == home {
		return "main"
	}
	name := filepath.Base(cwd)
	name = strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	if name == "" || name == "." {
		return "main"
	}
	return name
}

const (
	enterSeq = "\x1b[?1049h\x1b[2J\x1b[H\x1b[?1000h\x1b[?1006h"
	leaveSeq = "\x1b[?1000l\x1b[?1006l\x1b[0m\x1b[?1049l\x1b[?25h"
)

func runClient(sessions []string) error {
	cwd, _ := os.Getwd()
	if len(sessions) == 0 {
		sessions = []string{sessionNameFor(cwd)}
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal")
	}
	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return err
	}
	if cols < 20 || rows < 5 {
		return fmt.Errorf("terminal too small (%dx%d)", cols, rows)
	}

	mc, err := dialServer(true)
	if err != nil {
		return err
	}
	defer mc.Close()

	req, _ := json.Marshal(attachReq{Sessions: sessions, Cols: cols, Rows: rows, Cwd: cwd})
	if err := mc.writeMsg(msgAttach, req); err != nil {
		return err
	}

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	os.Stdout.WriteString(enterSeq)
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		os.Stdout.WriteString(leaveSeq)
		term.Restore(fd, oldState)
	}
	defer restore()

	// stdin -> server
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if mc.writeMsg(msgStdin, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// window resizes -> server
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if c, r, err := term.GetSize(fd); err == nil {
				p := make([]byte, 4)
				binary.BigEndian.PutUint16(p[0:], uint16(c))
				binary.BigEndian.PutUint16(p[2:], uint16(r))
				if mc.writeMsg(msgResize, p) != nil {
					return
				}
			}
		}
	}()

	// server -> stdout
	reason := "server connection lost"
	for {
		t, p, err := mc.readMsg()
		if err != nil {
			break
		}
		switch t {
		case msgOutput:
			os.Stdout.Write(p)
		case msgExit:
			reason = string(p)
			goto done
		}
	}
done:
	signal.Stop(winch)
	restore()
	fmt.Printf("[perch] %s\n", reason)
	return nil
}
