package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hinshun/vt10x"
)

type Server struct {
	mu       sync.Mutex
	sessions map[string]*Session
	clients  map[*client]bool
	ln       net.Listener
}

const (
	modeLive = iota
	modePalette
	modeScroll
)

// client is the server-side state for one attached terminal.
type client struct {
	mc  *msgConn
	srv *Server
	cwd string

	mu           sync.Mutex
	sess         *Session
	cols, rows   int
	mode         int
	anchor       int // scroll mode: absolute history index of the top line
	filter       []rune
	sel          int
	pend         []byte
	prev         []string
	palRows      []palRow
	statusRanges []clickRange

	renderCh chan struct{}
	quit     chan struct{}
	closed   bool
}

func runServer() error {
	log.SetPrefix("[vmux] ")
	if err := os.MkdirAll(vmuxDir(), 0o700); err != nil {
		return err
	}
	os.Remove(sockPath())
	ln, err := net.Listen("unix", sockPath())
	if err != nil {
		return err
	}
	srv := &Server{
		sessions: make(map[string]*Session),
		clients:  make(map[*client]bool),
		ln:       ln,
	}
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		srv.shutdown("server terminated")
	}()
	log.Printf("listening on %s", sockPath())
	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // listener closed during shutdown
		}
		go srv.handleConn(conn)
	}
}

func (srv *Server) shutdown(reason string) {
	srv.mu.Lock()
	for c := range srv.clients {
		go c.exit(reason)
	}
	for _, s := range srv.sessions {
		s.kill()
	}
	srv.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	os.Remove(sockPath())
	os.Exit(0)
}

func (srv *Server) handleConn(conn net.Conn) {
	mc := newMsgConn(conn)
	t, p, err := mc.readMsg()
	if err != nil {
		mc.Close()
		return
	}
	switch t {
	case msgList:
		mc.writeMsg(msgOutput, []byte(srv.listText()))
		mc.Close()
	case msgKill:
		mc.writeMsg(msgOutput, []byte("vmux server stopped\n"))
		mc.Close()
		srv.shutdown("server stopped")
	case msgAttach:
		var req attachReq
		if err := json.Unmarshal(p, &req); err != nil {
			mc.Close()
			return
		}
		srv.attach(mc, req)
	default:
		mc.Close()
	}
}

func (srv *Server) listText() string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.sessions) == 0 {
		return "no sessions\n"
	}
	var b strings.Builder
	for _, s := range srv.sessionsByName() {
		n := 0
		for c := range srv.clients {
			if c.sess == s {
				n++
			}
		}
		state := "detached"
		if n > 0 {
			state = fmt.Sprintf("%d attached", n)
		}
		fmt.Fprintf(&b, "%-20s %s\n", s.name, state)
	}
	return b.String()
}

func sanitizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, " ", "-")
	var b strings.Builder
	for _, r := range name {
		if r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "main"
	}
	return b.String()
}

// findOrCreate returns the named session, creating it if needed.
// Caller must NOT hold srv.mu.
func (srv *Server) findOrCreate(name, cwd string, cols, rows int) (*Session, error) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if s, ok := srv.sessions[name]; ok {
		return s, nil
	}
	s, err := newSession(name, cwd, cols, rows)
	if err != nil {
		return nil, err
	}
	srv.sessions[name] = s
	go s.readLoop(srv)
	log.Printf("session %q created", name)
	return s, nil
}

func (srv *Server) attach(mc *msgConn, req attachReq) {
	name := sanitizeName(req.Session)
	if req.Cols < 20 || req.Rows < 5 || req.Cols > 1000 || req.Rows > 1000 {
		mc.writeMsg(msgExit, []byte("bad terminal size"))
		mc.Close()
		return
	}
	c := &client{
		mc:       mc,
		srv:      srv,
		cwd:      req.Cwd,
		cols:     req.Cols,
		rows:     req.Rows,
		renderCh: make(chan struct{}, 1),
		quit:     make(chan struct{}),
	}
	s, err := srv.findOrCreate(name, req.Cwd, req.Cols, req.Rows-1)
	if err != nil {
		mc.writeMsg(msgExit, []byte("cannot start session: "+err.Error()))
		mc.Close()
		return
	}
	c.sess = s
	srv.mu.Lock()
	srv.clients[c] = true
	s.lastActive = time.Now()
	srv.mu.Unlock()

	s.resize(req.Cols, req.Rows-1)
	c.sendTitle()
	go c.renderLoop()
	c.scheduleRender()
	c.readLoop()
}

func (c *client) readLoop() {
	for {
		t, p, err := c.mc.readMsg()
		if err != nil {
			break
		}
		switch t {
		case msgStdin:
			c.handleInput(p)
		case msgResize:
			if len(p) == 4 {
				cols := int(binary.BigEndian.Uint16(p[0:]))
				rows := int(binary.BigEndian.Uint16(p[2:]))
				if cols >= 20 && rows >= 5 {
					c.mu.Lock()
					c.cols, c.rows = cols, rows
					c.prev = nil
					s := c.sess
					c.mu.Unlock()
					s.resize(cols, rows-1)
					c.scheduleRender()
				}
			}
		}
	}
	c.teardown()
}

func (c *client) teardown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	close(c.quit)
	c.mc.Close()
	c.srv.mu.Lock()
	delete(c.srv.clients, c)
	c.srv.mu.Unlock()
}

func (c *client) exit(reason string) {
	c.mc.writeMsg(msgExit, []byte(reason))
	c.teardown()
}

func (c *client) scheduleRender() {
	select {
	case c.renderCh <- struct{}{}:
	default:
	}
}

func (c *client) renderLoop() {
	for {
		select {
		case <-c.renderCh:
			c.render()
		case <-c.quit:
			return
		}
	}
}

func (c *client) render() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	var f *frame
	switch c.mode {
	case modeLive:
		f = c.renderLive()
	case modeScroll:
		f = c.renderScroll()
	case modePalette:
		f = c.renderPalette()
	}
	c.paint(f)
}

func (c *client) sendTitle() {
	c.mu.Lock()
	name := c.sess.name
	c.mu.Unlock()
	c.mc.writeMsg(msgOutput, []byte("\x1b]2;vmux · "+name+"\x07"))
}

// sessionsByName gives a stable ordering for the status bar.
// Caller must hold srv.mu.
func (srv *Server) sessionsByName() []*Session {
	out := make([]*Session, 0, len(srv.sessions))
	for _, s := range srv.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// sessionsByRecency orders sessions most-recently-used first, which makes the
// palette's default selection the "previous" session (alt-tab behavior).
func (srv *Server) sessionsByRecency() []*Session {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	out := make([]*Session, 0, len(srv.sessions))
	for _, s := range srv.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lastActive.After(out[j].lastActive) })
	return out
}

func (srv *Server) attachedCount(s *Session) int {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	n := 0
	for c := range srv.clients {
		if c.sess == s {
			n++
		}
	}
	return n
}

// filteredSessions applies the palette's fuzzy filter (subsequence match).
// Caller must hold c.mu.
func (c *client) filteredSessions() []*Session {
	all := c.srv.sessionsByRecency()
	if len(c.filter) == 0 {
		return all
	}
	pat := strings.ToLower(string(c.filter))
	var out []*Session
	for _, s := range all {
		if fuzzyMatch(strings.ToLower(s.name), pat) {
			out = append(out, s)
		}
	}
	return out
}

func fuzzyMatch(s, pat string) bool {
	i := 0
	for _, r := range s {
		if i < len(pat) && rune(pat[i]) == r {
			i++
		}
	}
	return i == len(pat)
}

// sessionUpdated is called from a session's readLoop after new output.
func (srv *Server) sessionUpdated(s *Session) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for c := range srv.clients {
		if c.sess == s && c.mode != modePalette {
			c.scheduleRender()
		}
	}
}

// sessionClosed is called when a session's shell exits.
func (srv *Server) sessionClosed(s *Session) {
	srv.mu.Lock()
	if srv.sessions[s.name] == s {
		delete(srv.sessions, s.name)
	}
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
	var affected []*client
	for c := range srv.clients {
		if c.sess == s {
			affected = append(affected, c)
		}
	}
	var next *Session
	for _, o := range srv.sessions {
		if next == nil || o.lastActive.After(next.lastActive) {
			next = o
		}
	}
	empty := len(srv.sessions) == 0
	srv.mu.Unlock()
	log.Printf("session %q closed", s.name)

	for _, c := range affected {
		if empty {
			c.exit("session closed · no sessions left")
		} else {
			c.switchTo(next)
		}
	}
	if empty {
		srv.mu.Lock()
		nClients := len(srv.clients)
		srv.mu.Unlock()
		if nClients == 0 {
			log.Printf("no sessions left, server exiting")
			os.Remove(sockPath())
			os.Exit(0)
		}
	}
}

func (c *client) switchTo(s *Session) {
	c.mu.Lock()
	c.sess = s
	c.mode = modeLive
	c.prev = nil
	c.filter = nil
	cols, rows := c.cols, c.rows
	c.mu.Unlock()
	c.srv.mu.Lock()
	s.lastActive = time.Now()
	c.srv.mu.Unlock()
	s.resize(cols, rows-1)
	c.sendTitle()
	c.scheduleRender()
}

func (c *client) createAndSwitch(name string) {
	c.mu.Lock()
	cols, rows := c.cols, c.rows
	cwd := c.cwd
	c.mu.Unlock()
	s, err := c.srv.findOrCreate(sanitizeName(name), cwd, cols, rows-1)
	if err != nil {
		log.Printf("create session: %v", err)
		return
	}
	c.switchTo(s)
}

// --- input handling ---

func (c *client) handleInput(p []byte) {
	c.mu.Lock()
	data := append(c.pend, p...)
	c.pend = nil
	mode := c.mode
	c.mu.Unlock()

	switch mode {
	case modeLive:
		evs, rest := tokenizeLive(data)
		if rest != nil {
			c.mu.Lock()
			c.pend = rest
			c.mu.Unlock()
		}
		for _, ev := range evs {
			c.liveEvent(ev)
		}
	case modePalette:
		for _, ev := range tokenizeKeys(data) {
			c.paletteEvent(ev)
		}
		c.scheduleRender()
	case modeScroll:
		for _, ev := range tokenizeKeys(data) {
			c.scrollEvent(ev)
		}
		c.scheduleRender()
	}
}

func (c *client) liveEvent(ev event) {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	switch ev.kind {
	case evRaw:
		data := ev.data
		s.mu.Lock()
		mode := s.vt.Mode()
		paste := s.hist.Paste
		s.mu.Unlock()
		if mode&vt10x.ModeAppCursor != 0 {
			data = translateArrows(data)
		}
		if !paste {
			data = stripPasteMarkers(data)
		}
		s.writeInput(data)
	case evKey:
		switch ev.key {
		case keyCtrlSpace:
			c.openPalette()
		case keyCtrlBackslash:
			c.exit("detached · sessions still running · reattach with: vmux")
		}
	case evMouse:
		c.liveMouse(ev.mouse, s)
	}
}

func (c *client) liveMouse(m mouseEv, s *Session) {
	c.mu.Lock()
	rows := c.rows
	ranges := c.statusRanges
	c.mu.Unlock()

	// clicks on the status bar switch sessions
	if m.y == rows && m.press && m.btn == 0 {
		for _, r := range ranges {
			if m.x >= r.x0 && m.x <= r.x1 {
				if t := c.srv.sessionByName(r.name); t != nil {
					c.switchTo(t)
				}
				return
			}
		}
		return
	}

	s.mu.Lock()
	vmode := s.vt.Mode()
	s.mu.Unlock()
	appMouse := vmode&vt10x.ModeMouseMask != 0
	appCursor := vmode&vt10x.ModeAppCursor != 0
	altScreen := vmode&vt10x.ModeAltScreen != 0

	if appMouse {
		if m.y < rows { // not our status row
			s.writeInput(m.raw)
		}
		return
	}
	switch m.btn {
	case mouseWheelUp:
		if altScreen {
			s.writeInput(wheelArrows(true, appCursor))
		} else if m.press {
			c.enterScroll()
		}
	case mouseWheelDown:
		if altScreen {
			s.writeInput(wheelArrows(false, appCursor))
		}
	}
}

func wheelArrows(up, appCursor bool) []byte {
	ch := byte('B')
	if up {
		ch = 'A'
	}
	mid := byte('[')
	if appCursor {
		mid = 'O'
	}
	seq := []byte{0x1b, mid, ch}
	out := make([]byte, 0, 9)
	for i := 0; i < 3; i++ {
		out = append(out, seq...)
	}
	return out
}

func stripPasteMarkers(p []byte) []byte {
	if !strings.Contains(string(p), "\x1b[200~") && !strings.Contains(string(p), "\x1b[201~") {
		return p
	}
	s := string(p)
	s = strings.ReplaceAll(s, "\x1b[200~", "")
	s = strings.ReplaceAll(s, "\x1b[201~", "")
	return []byte(s)
}

func (srv *Server) sessionByName(name string) *Session {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.sessions[name]
}

func (c *client) openPalette() {
	n := len(c.srv.sessionsByRecency())
	c.mu.Lock()
	c.mode = modePalette
	c.filter = nil
	c.prev = nil
	c.sel = 0
	if n > 1 {
		c.sel = 1 // default to the previous session: Ctrl-Space Enter = alt-tab
	}
	c.mu.Unlock()
	c.scheduleRender()
}

func (c *client) closeOverlay() {
	c.mu.Lock()
	c.mode = modeLive
	c.prev = nil
	c.mu.Unlock()
	c.scheduleRender()
}

func (c *client) paletteEvent(ev event) {
	switch ev.kind {
	case evKey:
		switch ev.key {
		case keyEsc, keyCtrlC:
			c.closeOverlay()
		case keyCtrlSpace: // double-tap: jump straight to previous session
			list := c.srv.sessionsByRecency()
			if len(list) > 1 {
				c.switchTo(list[1])
			} else {
				c.closeOverlay()
			}
		case keyEnter:
			c.mu.Lock()
			list := c.filteredSessions()
			sel := c.sel
			filter := string(c.filter)
			c.mu.Unlock()
			if len(list) > 0 {
				if sel >= len(list) {
					sel = 0
				}
				c.switchTo(list[sel])
			} else if filter != "" {
				c.createAndSwitch(filter)
			}
		case keyUp, keyCtrlP:
			c.mu.Lock()
			c.sel--
			if c.sel < 0 {
				c.sel = 0
			}
			c.mu.Unlock()
		case keyDown, keyCtrlN:
			c.mu.Lock()
			c.sel++
			c.mu.Unlock()
		case keyBackspace:
			c.mu.Lock()
			if len(c.filter) > 0 {
				c.filter = c.filter[:len(c.filter)-1]
			}
			c.mu.Unlock()
		case keyCtrlU:
			c.mu.Lock()
			c.filter = nil
			c.mu.Unlock()
		case keyCtrlD:
			c.exit("detached · sessions still running · reattach with: vmux")
		case keyCtrlX:
			c.mu.Lock()
			list := c.filteredSessions()
			sel := c.sel
			c.mu.Unlock()
			if sel < len(list) {
				list[sel].kill()
			}
		}
	case evRune:
		c.mu.Lock()
		if ev.r >= '1' && ev.r <= '9' && len(c.filter) == 0 {
			idx := int(ev.r - '1')
			list := c.filteredSessions()
			c.mu.Unlock()
			if idx < len(list) {
				c.switchTo(list[idx])
			}
			return
		}
		c.filter = append(c.filter, ev.r)
		c.sel = 0
		c.mu.Unlock()
	case evMouse:
		if ev.mouse.press && ev.mouse.btn == 0 {
			c.mu.Lock()
			var target string
			for _, pr := range c.palRows {
				if pr.y == ev.mouse.y {
					target = pr.name
				}
			}
			c.mu.Unlock()
			if target != "" {
				if t := c.srv.sessionByName(target); t != nil {
					c.switchTo(t)
				}
			}
		}
	}
}

func (c *client) enterScroll() {
	c.mu.Lock()
	s := c.sess
	rows := c.rows
	c.mu.Unlock()
	s.mu.Lock()
	total := s.hist.Total()
	first := s.hist.First()
	s.mu.Unlock()
	if total == first {
		return // no history yet
	}
	anchor := total - (rows - 1) - 3
	if anchor < first {
		anchor = first
	}
	c.mu.Lock()
	c.mode = modeScroll
	c.anchor = anchor
	c.prev = nil
	c.mu.Unlock()
	c.scheduleRender()
}

func (c *client) scrollBy(delta int) {
	c.mu.Lock()
	s := c.sess
	rows := c.rows
	c.mu.Unlock()
	s.mu.Lock()
	total := s.hist.Total()
	first := s.hist.First()
	s.mu.Unlock()
	bottom := total - (rows - 1)
	if bottom < first {
		bottom = first
	}
	c.mu.Lock()
	c.anchor += delta
	if c.anchor < first {
		c.anchor = first
	}
	exit := c.anchor >= bottom && delta > 0
	if c.anchor > bottom {
		c.anchor = bottom
	}
	c.mu.Unlock()
	if exit {
		c.closeOverlay()
	}
}

func (c *client) scrollEvent(ev event) {
	switch ev.kind {
	case evKey:
		switch ev.key {
		case keyEsc, keyEnter, keyCtrlC:
			c.closeOverlay()
		case keyUp:
			c.scrollBy(-1)
		case keyDown:
			c.scrollBy(1)
		case keyPgUp:
			c.scrollBy(-(c.rows - 2))
		case keyPgDn:
			c.scrollBy(c.rows - 2)
		case keyHome:
			c.mu.Lock()
			c.anchor = 0
			c.mu.Unlock()
			c.scrollBy(0)
		case keyEnd:
			c.scrollBy(1 << 30)
		}
	case evRune:
		switch ev.r {
		case 'q':
			c.closeOverlay()
		case 'k':
			c.scrollBy(-1)
		case 'j':
			c.scrollBy(1)
		case 'g':
			c.mu.Lock()
			c.anchor = 0
			c.mu.Unlock()
			c.scrollBy(0)
		case 'G':
			c.scrollBy(1 << 30)
		}
	case evMouse:
		switch ev.mouse.btn {
		case mouseWheelUp:
			c.scrollBy(-3)
		case mouseWheelDown:
			c.scrollBy(3)
		}
	}
}
