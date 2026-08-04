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

const usage = `vmux: a friendlier terminal multiplexer

usage:
  vmux [session]      attach to a session (creates it if missing; default "main")
  vmux ls             list sessions
  vmux kill-server    stop the server and all sessions

inside vmux:
  Ctrl-Space          session palette: type to filter or name a new session,
                      Enter to switch/create, 1-9 to jump, Ctrl-x kill, Esc close
  Ctrl-\              detach (sessions keep running)
  mouse wheel         scroll history; q or Esc returns to live
  status bar clicks   click a session name to switch
`

func vmuxDir() string {
	if d := os.Getenv("VMUX_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return filepath.Join(home, ".vmux")
}

func sockPath() string { return filepath.Join(vmuxDir(), "vmux.sock") }

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "--server":
			if err := runServer(); err != nil {
				fmt.Fprintln(os.Stderr, "vmux server:", err)
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
	session := ""
	if len(args) > 0 {
		session = args[0]
	}
	if os.Getenv("VMUX") != "" {
		fmt.Fprintln(os.Stderr, "vmux: already inside a vmux session (unset VMUX to force)")
		os.Exit(1)
	}
	if err := runClient(session); err != nil {
		fmt.Fprintln(os.Stderr, "vmux:", err)
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
		return nil, fmt.Errorf("no vmux server running")
	}
	if err := os.MkdirAll(vmuxDir(), 0o700); err != nil {
		return nil, err
	}
	os.Remove(sockPath()) // stale socket from a dead server
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(vmuxDir(), "server.log"),
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
	return nil, fmt.Errorf("server failed to start (see %s)", filepath.Join(vmuxDir(), "server.log"))
}

// runControl sends a single control message and prints any reply.
func runControl(typ byte) {
	mc, err := dialServer(false)
	if err != nil {
		if typ == msgList {
			fmt.Println("no server running, no sessions")
			return
		}
		fmt.Fprintln(os.Stderr, "vmux:", err)
		os.Exit(1)
	}
	defer mc.Close()
	if err := mc.writeMsg(typ, nil); err != nil {
		fmt.Fprintln(os.Stderr, "vmux:", err)
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
