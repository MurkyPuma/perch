package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
)

// Wire format, both directions: [type: 1 byte][payload length: uint32 BE][payload].
const (
	msgAttach = 'a' // client -> server: JSON attachReq
	msgStdin  = 'd' // client -> server: raw terminal input bytes
	msgResize = 'r' // client -> server: cols, rows as uint16 BE pairs
	msgList   = 'l' // client -> server: request session list, server replies with output then closes
	msgKill   = 'q' // client -> server: shut the server down
	msgOutput = 'o' // server -> client: raw bytes to write to the real terminal
	msgExit   = 'x' // server -> client: reason string, connection is done
)

type attachReq struct {
	// Sessions to show side by side (up to maxTiles); first is focused.
	Sessions []string `json:"sessions"`
	Cols     int      `json:"cols"`
	Rows     int      `json:"rows"`
	Cwd      string   `json:"cwd"`
}

const maxPayload = 16 << 20

// msgConn wraps a net.Conn with a write lock so multiple server goroutines
// (session output pump, control replies) can safely interleave frames.
type msgConn struct {
	conn net.Conn
	wmu  sync.Mutex
}

func newMsgConn(c net.Conn) *msgConn { return &msgConn{conn: c} }

func (m *msgConn) writeMsg(typ byte, payload []byte) error {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	hdr := make([]byte, 5)
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := m.conn.Write(hdr); err != nil {
		return err
	}
	_, err := m.conn.Write(payload)
	return err
}

func (m *msgConn) readMsg() (byte, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(m.conn, hdr); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxPayload {
		return 0, nil, fmt.Errorf("payload too large: %d", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(m.conn, p); err != nil {
		return 0, nil, err
	}
	return hdr[0], p, nil
}

func (m *msgConn) Close() error { return m.conn.Close() }
