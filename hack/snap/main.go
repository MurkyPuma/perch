// Command snap produces the README screenshots. It runs a scripted vmux
// session under a real PTY, mirrors the client's output into a vt10x screen,
// and dumps styled HTML snapshots that headless Chrome renders to PNG.
//
// usage: go run ./hack/snap <outdir>
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

const (
	cols = 104
	rows = 24
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: snap <outdir>")
		os.Exit(1)
	}
	outdir := os.Args[1]
	if err := run(outdir); err != nil {
		fmt.Fprintln(os.Stderr, "snap:", err)
		os.Exit(1)
	}
}

func run(outdir string) error {
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "vmux-snap")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	// isolated vmux server + a controlled zsh prompt for pretty output
	zdot := filepath.Join(work, "zdot")
	os.MkdirAll(zdot, 0o755)
	os.WriteFile(filepath.Join(zdot, ".zshrc"),
		[]byte("PROMPT='%F{213}➜%f %F{81}%1~%f '\nalias ls='ls -G'\n"), 0o644)

	env := append(os.Environ(),
		"VMUX_DIR="+filepath.Join(work, "state"),
		"SHELL=/bin/zsh",
		"ZDOTDIR="+zdot,
		"CLICOLOR=1",
	)
	bin := filepath.Join(work, "vmux")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		return fmt.Errorf("build: %v\n%s", err, out)
	}
	defer func() {
		kill := exec.Command(bin, "kill-server")
		kill.Env = env
		kill.Run()
	}()

	demo := makeDemoDirs(work)

	// seed two background sessions, then attach the one we screenshot
	for _, name := range []string{"claimable", "vibe"} {
		if err := seedSession(bin, env, demo[name], name); err != nil {
			return err
		}
		lsCmd := exec.Command(bin, "ls")
		lsCmd.Env = env
		out, _ := lsCmd.CombinedOutput()
		if !strings.Contains(string(out), name) {
			return fmt.Errorf("seeding %s failed, sessions:\n%s", name, out)
		}
	}

	cmd := exec.Command(bin, "soda-vault")
	cmd.Env = env
	cmd.Dir = demo["soda-vault"]
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	defer func() { ptmx.Write([]byte{0x1c}); time.Sleep(200 * time.Millisecond); ptmx.Close() }()

	vt := vt10x.New(vt10x.WithSize(cols, rows))
	go func() {
		b := make([]byte, 8192)
		for {
			n, err := ptmx.Read(b)
			if n > 0 {
				vt.Write(b[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	type step struct {
		send string
		wait time.Duration
		shot string
	}
	steps := []step{
		{wait: 1200 * time.Millisecond},
		{send: "ls\r", wait: 700 * time.Millisecond},
		{send: "./scripts/dev.sh\r", wait: 900 * time.Millisecond, shot: "live"},
		{send: "\x00", wait: 600 * time.Millisecond, shot: "palette"},
		{send: "\x1b", wait: 400 * time.Millisecond},
		{send: "seq 12 400\r", wait: 900 * time.Millisecond},
		{send: "\x1b[<64;40;10M\x1b[<64;40;10M\x1b[<64;40;10M", wait: 200 * time.Millisecond},
		{send: "\x1b[<64;40;10M\x1b[<64;40;10M", wait: 600 * time.Millisecond, shot: "scroll"},
		{send: "q", wait: 300 * time.Millisecond},
	}
	for _, st := range steps {
		if st.send != "" {
			if _, err := ptmx.Write([]byte(st.send)); err != nil {
				return err
			}
		}
		time.Sleep(st.wait)
		if st.shot != "" {
			path := filepath.Join(outdir, st.shot+".html")
			if err := os.WriteFile(path, []byte(snapshotHTML(vt)), 0o644); err != nil {
				return err
			}
			fmt.Println("wrote", path)
		}
	}
	return nil
}

// makeDemoDirs builds small fake project directories so ls output looks like
// real projects without exposing anything private.
func makeDemoDirs(work string) map[string]string {
	dirs := map[string][]string{
		"soda-vault": {"api/", "app/", "firmware/", "scripts/", "docker-compose.yml", "Makefile", "README.md"},
		"claimable":  {"server/", "web/", "deploy/", "README.md"},
		"vibe":       {"Vibe.xcodeproj/", "Sources/", "README.md"},
	}
	out := map[string]string{}
	for name, entries := range dirs {
		root := filepath.Join(work, "demo", name)
		os.MkdirAll(root, 0o755)
		for _, e := range entries {
			if strings.HasSuffix(e, "/") {
				os.MkdirAll(filepath.Join(root, strings.TrimSuffix(e, "/")), 0o755)
			} else {
				os.WriteFile(filepath.Join(root, e), []byte("demo\n"), 0o644)
			}
		}
		out[name] = root
	}
	// a colorful fake dev-server script for the live screenshot
	script := filepath.Join(work, "demo", "soda-vault", "scripts", "dev.sh")
	os.WriteFile(script, []byte(`#!/bin/sh
printf '\033[90m$ docker compose up api\033[0m\n'
printf '\033[32m✓\033[0m api        \033[90mlistening on\033[0m \033[36mhttp://localhost:8000\033[0m\n'
printf '\033[32m✓\033[0m postgres   \033[90mready\033[0m\n'
printf '\033[32m✓\033[0m worker     \033[90m4 queues\033[0m\n'
printf '\033[33m…\033[0m watching for changes\n'
`), 0o755)
	return out
}

func seedSession(bin string, env []string, dir, name string) error {
	cmd := exec.Command(bin, name)
	cmd.Env = env
	cmd.Dir = dir
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	go func() {
		b := make([]byte, 4096)
		for {
			if _, err := ptmx.Read(b); err != nil {
				return
			}
		}
	}()
	time.Sleep(900 * time.Millisecond)
	ptmx.Write([]byte{0x1c}) // detach, session stays alive
	time.Sleep(300 * time.Millisecond)
	ptmx.Close()
	cmd.Wait()
	return nil
}

// --- vt10x screen -> HTML ---

var base16 = []string{
	"#3f4451", "#e05561", "#8cc265", "#d18f52", "#4aa5f0", "#c162de", "#42b3c2", "#d7dae0",
	"#4f5666", "#ff616e", "#a5e075", "#f0a45d", "#4dc4ff", "#de73ff", "#4cd1e0", "#e6e6e6",
}

const (
	defaultFG = "#abb2bf"
	defaultBG = "#282c34"
)

func cssColor(c vt10x.Color, fg bool) string {
	switch {
	case c == vt10x.DefaultFG:
		return defaultFG
	case c == vt10x.DefaultBG, c == vt10x.DefaultCursor:
		return defaultBG
	case c < 16:
		return base16[c]
	case c < 232: // 6x6x6 cube
		v := []int{0, 95, 135, 175, 215, 255}
		i := int(c) - 16
		return fmt.Sprintf("#%02x%02x%02x", v[i/36], v[i/6%6], v[i%6])
	case c < 256: // grayscale ramp
		g := 8 + (int(c)-232)*10
		return fmt.Sprintf("#%02x%02x%02x", g, g, g)
	default: // truecolor packed
		return fmt.Sprintf("#%06x", uint32(c)&0xffffff)
	}
}

func snapshotHTML(vt vt10x.Terminal) string {
	var rowsHTML strings.Builder
	vt.Lock()
	curs := vt.Cursor()
	cursVis := vt.CursorVisible()
	for y := 0; y < rows; y++ {
		var line strings.Builder
		lastFG, lastBG := "", ""
		open := false
		for x := 0; x < cols; x++ {
			g := vt.Cell(x, y)
			fg, bg := cssColor(g.FG, true), cssColor(g.BG, false)
			if cursVis && x == curs.X && y == curs.Y {
				fg, bg = defaultBG, defaultFG // block cursor
			}
			if fg != lastFG || bg != lastBG {
				if open {
					line.WriteString("</span>")
				}
				fmt.Fprintf(&line, `<span style="color:%s;background:%s">`, fg, bg)
				open = true
				lastFG, lastBG = fg, bg
			}
			ch := g.Char
			if ch == 0 {
				ch = ' '
			}
			switch ch {
			case '<':
				line.WriteString("&lt;")
			case '>':
				line.WriteString("&gt;")
			case '&':
				line.WriteString("&amp;")
			default:
				line.WriteRune(ch)
			}
		}
		if open {
			line.WriteString("</span>")
		}
		rowsHTML.WriteString(line.String())
		rowsHTML.WriteString("\n")
	}
	vt.Unlock()

	return `<!doctype html><meta charset="utf-8"><style>
  html,body{margin:0;padding:0;background:transparent}
  body{padding:28px;display:inline-block}
  .win{border-radius:12px;overflow:hidden;background:` + defaultBG + `;
       box-shadow:0 18px 50px rgba(0,0,0,.5);border:1px solid #3a3f4b;display:inline-block}
  .bar{height:34px;background:#21252b;display:flex;align-items:center;padding:0 14px;gap:8px}
  .dot{width:12px;height:12px;border-radius:50%}
  pre{margin:0;padding:14px 16px;font:13px/1.35 "SF Mono",Menlo,monospace;color:` + defaultFG + `}
</style><body><div class="win">
<div class="bar"><span class="dot" style="background:#ff5f57"></span><span class="dot" style="background:#febc2e"></span><span class="dot" style="background:#28c840"></span></div>
<pre>` + rowsHTML.String() + `</pre></div></body>`
}
