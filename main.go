package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const usage = `perch: a friendlier terminal multiplexer

usage:
  perch                attach to a session named after the current directory
  perch api            attach to the session "api" (creates it if missing)
  perch api web        open "api" and "web" side by side (up to 3)
  perch ls             list sessions
  perch kill-server    stop the server and all sessions

inside perch:
  Ctrl-Space          session palette: type to filter or name a new session,
                      Enter to switch/create, 1-9 to jump, Ctrl-x kill, Esc close
  Ctrl-S (palette)    split: show the selected session next to the current one
  Ctrl-L (palette)    flip splits between side by side and stacked top/bottom
  Ctrl-O              cycle focus between split tiles
  Ctrl-\              detach (sessions keep running)
  mouse wheel         scroll history; q or Esc returns to live
  status bar clicks   click a session name to focus or switch
`

func perchDir() string {
	if d := os.Getenv("PERCH_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return filepath.Join(home, ".perch")
}

func sockPath() string { return filepath.Join(perchDir(), "perch.sock") }

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "--server":
			if err := runServer(); err != nil {
				fmt.Fprintln(os.Stderr, "perch server:", err)
				os.Exit(1)
			}
			return
		case "ls":
			runControl(msgList)
			return
		case "kill-server":
			runControl(msgKill)
			return
		case "-h", "--help", "help":
			fmt.Print(usage)
			return
		}
	}
	if os.Getenv("PERCH") != "" {
		fmt.Fprintln(os.Stderr, "perch: already inside a perch session (unset PERCH to force)")
		os.Exit(1)
	}
	if err := runClient(args); err != nil {
		fmt.Fprintln(os.Stderr, "perch:", err)
		os.Exit(1)
	}
}

// dialServer connects to a running server, starting one if needed.
func dialServer(autostart bool) (*msgConn, error) {
	conn, err := net.Dial("unix", sockPath())
	if err == nil {
		return newMsgConn(conn), nil
	}
	if !autostart {
		return nil, fmt.Errorf("no perch server running")
	}
	if err := os.MkdirAll(perchDir(), 0o700); err != nil {
		return nil, err
	}
	os.Remove(sockPath()) // stale socket from a dead server
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(perchDir(), "server.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "--server")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait() // reap if it ever exits while we're alive
	logf.Close()
	for i := 0; i < 250; i++ {
		time.Sleep(40 * time.Millisecond)
		if conn, err := net.Dial("unix", sockPath()); err == nil {
			return newMsgConn(conn), nil
		}
	}
	return nil, fmt.Errorf("server failed to start (see %s)", filepath.Join(perchDir(), "server.log"))
}

// runControl sends a single control message and prints any reply.
func runControl(typ byte) {
	mc, err := dialServer(false)
	if err != nil {
		if typ == msgList {
			fmt.Println("no server running, no sessions")
			return
		}
		fmt.Fprintln(os.Stderr, "perch:", err)
		os.Exit(1)
	}
	defer mc.Close()
	if err := mc.writeMsg(typ, nil); err != nil {
		fmt.Fprintln(os.Stderr, "perch:", err)
		os.Exit(1)
	}
	for {
		t, p, err := mc.readMsg()
		if err != nil {
			return
		}
		if t == msgOutput {
			os.Stdout.Write(p)
		}
	}
}
