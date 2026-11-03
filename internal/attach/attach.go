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
// Newer abilities of a shim are listed in its Info frames (Features);
// longarmd uses them only with a shim that lists them.
package attach

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
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
	// SendX is Send with options (SendXPayload); shims list FeatureSendX.
	SendX byte = 'S'
)

// Shim features (InfoState.Features).
const (
	FeatureSendX = "send-x" // SendX frames: IfInputSeq, an after part
	// Input frames that are only terminal replies (IsReply) are not
	// counted as input; InfoState.HumanInputSeq.
	FeatureReplies = "replies"
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
	SourceSteer  = "steer"  // a steerable run's own keystrokes (start-up screens, /exit)
	// SourceReply: an attached client's terminal replies (IsReply),
	// written to the terminal but not counted as input.
	SourceReply = "attach-reply"
)

// MaxSend bounds the bytes of one Send.
const MaxSend = 256 << 10

// InfoState is the shim's view of the terminal's activity.
type InfoState struct {
	InputSeq uint64 `json:"inputSeq"` // writes to the terminal so far, terminal replies not counted
	// HumanInputSeq counts the attached clients' keystrokes only (Input
	// frames that are not terminal replies); with FeatureReplies.
	HumanInputSeq   uint64    `json:"humanInputSeq,omitempty"`
	LastInput       time.Time `json:"lastInput,omitzero"`
	LastInputSource string    `json:"lastInputSource,omitempty"`
	LastOutput      time.Time `json:"lastOutput,omitzero"`
	// Send answers: whether the bytes were written (false: input arrived
	// within the idle window).
	Sent bool `json:"sent,omitempty"`
	// Error: the terminal write failed or was short (Sent is false; some
	// bytes may have been written).
	Error string `json:"error,omitempty"`
	// Features: what the shim supports beyond Send (FeatureSendX, ...).
	Features []string `json:"features,omitempty"`
}

// Has reports whether the shim lists feature f.
func (st InfoState) Has(f string) bool { return slices.Contains(st.Features, f) }

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

// SendOpts are the options of a SendX frame.
type SendOpts struct {
	IfIdleMs uint32 `json:"ifIdleMs,omitempty"`
	Source   string `json:"source"`
	// IfInputSeq: write only if InputSeq is still this value.
	IfInputSeq *uint64 `json:"ifInputSeq,omitempty"`
	// AfterMs: the after part is written this long after the data, with
	// no other input in between.
	AfterMs uint32 `json:"afterMs,omitempty"`
	After   int    `json:"after,omitempty"` // the after part's length (set by SendXPayload)
}

// SendXPayload encodes a SendX frame: [opts len uint32][JSON opts][data][after].
func SendXPayload(o SendOpts, data, after []byte) []byte {
	o.After = len(after)
	j, _ := json.Marshal(o)
	b := make([]byte, 4, 4+len(j)+len(data)+len(after))
	binary.BigEndian.PutUint32(b, uint32(len(j)))
	b = append(b, j...)
	b = append(b, data...)
	return append(b, after...)
}

// ParseSendX decodes a SendX frame.
func ParseSendX(p []byte) (o SendOpts, data, after []byte, err error) {
	if len(p) < 4 || uint64(len(p)) < 4+uint64(binary.BigEndian.Uint32(p)) {
		return o, nil, nil, errors.New("attach: short send frame")
	}
	n := 4 + int(binary.BigEndian.Uint32(p))
	if err := json.Unmarshal(p[4:n], &o); err != nil {
		return o, nil, nil, fmt.Errorf("attach: send options: %w", err)
	}
	rest := p[n:]
	if o.After < 0 || o.After > len(rest) {
		return o, nil, nil, errors.New("attach: bad send frame")
	}
	return o, rest[:len(rest)-o.After], rest[len(rest)-o.After:], nil
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
