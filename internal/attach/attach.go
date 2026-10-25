// Package attach is the wire protocol of a session shim's attach channel
// (named pipe on Windows, unix socket elsewhere; see platform.ListenLocal):
// longarmd connects to it on behalf of each attached longarm client.
//
// Frames are [type byte][len uint32 big-endian][payload]. The client sends
// Hello first, then Input / Resize frames; the shim sends Output frames (the
// scrollback replay first, if requested) and finally Exit when the session's
// process has ended. Closing the connection detaches.
package attach

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Frame types.
const (
	Hello  byte = 'h' // client: payload [flags byte] (FlagReplay)
	Input  byte = 'i' // client: keyboard bytes
	Resize byte = 'r' // client: [cols uint16][rows uint16]
	Output byte = 'o' // shim: terminal bytes
	Exit   byte = 'x' // shim: [code int32]; the shim closes the connection after it
)

// FlagReplay asks the shim to send its scrollback ring before live output.
const FlagReplay byte = 1

// MaxFrame bounds a frame payload.
const MaxFrame = 1 << 20

// Conn frames a stream. WriteFrame is safe for concurrent use; ReadFrame is
// not (one reader).
type Conn struct {
	longarm io.ReadWriteCloser
	r       *bufio.Reader
	wmu     sync.Mutex
}

// NewConn wraps a stream.
func NewConn(longarm io.ReadWriteCloser) *Conn {
	return &Conn{longarm: longarm, r: bufio.NewReaderSize(longarm, 64<<10)}
}

// WriteFrame sends one frame.
func (c *Conn) WriteFrame(typ byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("attach: frame too large (%d)", len(payload))
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.longarm.Write(hdr[:]); err != nil {
		return err
	}
	_, err := c.longarm.Write(payload)
	return err
}

// ReadFrame reads one frame. The payload is valid until the next call.
func (c *Conn) ReadFrame() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("attach: frame too large (%d)", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(c.r, p); err != nil {
		return 0, nil, err
	}
	return hdr[0], p, nil
}

// Close closes the stream.
func (c *Conn) Close() error { return c.longarm.Close() }

// ResizePayload encodes a terminal size.
func ResizePayload(cols, rows uint16) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[:], cols)
	binary.BigEndian.PutUint16(b[2:], rows)
	return b[:]
}

// ParseResize decodes a terminal size.
func ParseResize(p []byte) (cols, rows uint16, ok bool) {
	if len(p) != 4 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(p), binary.BigEndian.Uint16(p[2:]), true
}

// ExitPayload encodes an exit code.
func ExitPayload(code int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	return b[:]
}

// ParseExit decodes an exit code.
func ParseExit(p []byte) int {
	if len(p) != 4 {
		return -1
	}
	return int(int32(binary.BigEndian.Uint32(p)))
}
