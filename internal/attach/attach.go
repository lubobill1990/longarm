// Package attach is the wire protocol of a session shim's attach channel
// (named pipe on Windows, unix socket elsewhere; see platform.ListenLocal):
// longarmd connects to it on behalf of each attached longarm client.
//
// Frames are [type byte][len uint32 big-endian][payload]. The client sends
// Hello first, then Input / Resize frames; the shim sends Output frames (the
// scrollback replay first, if requested) and finally Exit when the session's
// process has ended. Closing the connection detaches.
//
// A control connection (Hello with FlagControl) gets no output: the shim
// answers it with an Info frame at once, then one Info frame per Query or
// Send. Shims that predate control connections treat it as an ordinary
// client and never send Info, which is how longarmd tells them apart.
package attach

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Frame types.
const (
	Hello  byte = 'h' // client: payload [flags byte] (FlagReplay)
	Input  byte = 'i' // client: keyboard bytes
	Resize byte = 'r' // client: [cols uint16][rows uint16]
	Output byte = 'o' // shim: terminal bytes
	Exit   byte = 'x' // shim: [code int32]; the shim closes the connection after it

	// Control connections only.
	Query byte = 'q' // client: empty; the shim answers with Info
	Send  byte = 's' // client: SendPayload; the shim answers with Info (Sent)
	Info  byte = 'n' // shim: JSON InfoState
)

// Hello flags.
const (
	FlagReplay  byte = 1 // send the scrollback ring before live output
	FlagControl byte = 2 // a control connection: no output, Info / Query / Send
)

// Input sources (InfoState.LastInputSource).
const (
	SourceAttach = "attach" // an attached client's keystrokes (Input frames)
	SourceSend   = "send"   // Sessions.Send
	SourceStop   = "stop"   // Sessions.Stop's Ctrl-C
)

// MaxSend bounds the bytes of one Send.
const MaxSend = 256 << 10

// InfoState is the shim's view of the terminal's activity.
type InfoState struct {
	InputSeq        uint64    `json:"inputSeq"` // writes to the terminal so far
	LastInput       time.Time `json:"lastInput,omitzero"`
	LastInputSource string    `json:"lastInputSource,omitempty"`
	LastOutput      time.Time `json:"lastOutput,omitzero"`
	// Send answers: whether the bytes were written (false: input arrived
	// within the idle window).
	Sent bool `json:"sent,omitempty"`
}

// SendPayload encodes a Send frame: [ifIdleMs uint32][source len byte][source][data].
func SendPayload(ifIdleMs uint32, source string, data []byte) []byte {
	b := make([]byte, 5, 5+len(source)+len(data))
	binary.BigEndian.PutUint32(b, ifIdleMs)
	b[4] = byte(len(source))
	b = append(b, source...)
	return append(b, data...)
}

// ParseSend decodes a Send frame.
func ParseSend(p []byte) (ifIdleMs uint32, source string, data []byte, err error) {
	if len(p) < 5 || len(p) < 5+int(p[4]) {
		return 0, "", nil, errors.New("attach: short send frame")
	}
	n := int(p[4])
	return binary.BigEndian.Uint32(p), string(p[5 : 5+n]), p[5+n:], nil
}

// EncodeInfo / DecodeInfo convert Info frames.
func EncodeInfo(st InfoState) []byte {
	b, _ := json.Marshal(st)
	return b
}

func DecodeInfo(p []byte) (InfoState, error) {
	var st InfoState
	err := json.Unmarshal(p, &st)
	return st, err
}

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
