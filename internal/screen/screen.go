// Package screen is the OS side of Longarm's remote desktop
// (docs/remote-desktop.md): capturing a display (Source), injecting input
// (Injector), H.264 encoding and decoding, and the viewer window
// (RunViewer). Each OS implements it in its own files without cgo
// (Windows: DXGI, Media Foundation, D3D11 through COM vtables; macOS and
// Linux: see their files). Coordinates are physical pixels.
package screen

import (
	"errors"
	"time"
)

// ErrUnsupported: this OS (or this build) cannot do it.
var ErrUnsupported = errors.New("screen: not supported here")

// ErrAccessLost: the source cannot capture any more (the desktop switched
// to a secure screen it may not see, the resolution changed, the session
// went away); open it again.
var ErrAccessLost = errors.New("screen: capture lost (desktop switch or mode change)")

// Display is one monitor of the desktop.
type Display struct {
	Index      int
	Name       string
	X, Y, W, H int // desktop coordinates
}

// Rect is an area of a frame.
type Rect struct{ X, Y, W, H int }

// Cursor is the pointer as the source last saw it. ShapeID changes when
// the shape does; BGRA (straight alpha, W*H*4) is set when it changed.
type Cursor struct {
	X, Y       int // in the display's pixels
	Visible    bool
	ShapeID    uint64
	W, H       int
	HotX, HotY int
	BGRA       []byte
}

// Source captures one display.
type Source interface {
	Display() Display
	// Size is the frame's size (the display's, in pixels).
	Size() (w, h int)
	// Next waits up to timeout for the screen to change and updates Frame;
	// changed is false on a timeout. ErrAccessLost: open a new Source.
	Next(timeout time.Duration) (changed bool, err error)
	// Frame is the image (BGRA, stride W*4), valid until the next Next.
	Frame() []byte
	// Cursor is the pointer at the last Next.
	Cursor() Cursor
	Close()
}

// OpenSource starts capturing display (0: the primary). The goroutine
// that opens it must use it and close it (some OSes tie it to a thread).
func OpenSource(display int) (Source, error) { return openSource(display) }

// Available says why this process cannot show its desktop ("" = it can).
func Available() string { return available() }

// Mouse buttons for Injector.Button (the wire's numbering).
const (
	ButtonLeft   = 1
	ButtonMiddle = 2
	ButtonRight  = 3
)

// Injector types and clicks into the desktop. Use it from one goroutine.
type Injector interface {
	// Move puts the pointer at desktop coordinates.
	Move(x, y int) error
	Button(button int, down bool) error
	// Wheel scrolls (120 = one notch; positive: up / right).
	Wheel(dx, dy int) error
	// Key presses or releases a PC/AT set-1 scan code (0xE0xx: extended).
	Key(scan uint16, down bool) error
	// Text types Unicode text.
	Text(s string) error
	// SecureAttention is Ctrl+Alt+Del (only where a privileged component
	// may send it; ErrUnsupported elsewhere).
	SecureAttention() error
	Close()
}

// NewInjector returns this OS's input injector.
func NewInjector() (Injector, error) { return newInjector() }

// EncoderOptions configure a VideoEncoder.
type EncoderOptions struct {
	Width, Height int // even
	FPS           int // nominal; frames come when the screen changes
	Bitrate       int // bits/s
	Software      bool
}

// VideoEncoder turns BGRA frames into H.264 access units (Annex B). Use it
// from one goroutine.
type VideoEncoder interface {
	// Name says what encodes: "hardware" or "software".
	Name() string
	// Encode encodes one frame (BGRA, stride Width*4); the result can be
	// empty while the encoder fills its pipeline.
	Encode(bgra []byte) ([]byte, error)
	ForceKeyframe()
	Close()
}

// NewH264Encoder opens this OS's H.264 encoder (the GPU's first).
func NewH264Encoder(o EncoderOptions) (VideoEncoder, error) { return newH264Encoder(o) }

// VideoDecoder turns H.264 access units into BGRA images in memory (the
// viewer's software path and headless checks; RunViewer decodes on the
// GPU where it can). Use it from one goroutine.
type VideoDecoder interface {
	// Decode decodes one access unit; img is nil while the decoder needs
	// more input. img is valid until the next Decode.
	Decode(au []byte) (img []byte, w, h int, err error)
	Close()
}

// NewH264Decoder opens this OS's H.264 decoder for pictures of w×h.
func NewH264Decoder(w, h int) (VideoDecoder, error) { return newH264Decoder(w, h) }

// VideoPacket is what the viewer gets from the worker.
type VideoPacket struct {
	// Config: a new picture (size and codec); the next packet is a key frame.
	Config         bool
	Width, Height  int
	Codec          string // "h264" | "jpeg"
	Data           []byte // h264: an access unit
	Tiles          []Tile // jpeg
	Key            bool
	Captured       time.Time
	Seq            uint64
	Cursor         *Cursor // a pointer update (no picture)
	SecureHidden   bool    // the worker shows a screen the capturer cannot
	SecureHiddenOK bool    // SecureHidden is set in this packet
}

// Tile is a JPEG part of a frame.
type Tile struct {
	X, Y, W, H int
	JPEG       []byte
}

// InputEvent is what the viewer sends: exactly one of the parts is set.
type InputEvent struct {
	Mouse *MouseEvent
	Key   *KeyEvent
	Text  string
	SAS   bool // Ctrl+Alt+Del
}

// MouseEvent: a move (Button 0), a button, or the wheel, in the remote
// display's pixels.
type MouseEvent struct {
	X, Y           int
	Button         int
	Down           bool
	WheelX, WheelY int
}

// KeyEvent is a set-1 scan code.
type KeyEvent struct {
	Scan uint16
	Down bool
}

// ViewerOptions configure RunViewer.
type ViewerOptions struct {
	Title string
	// Packets come from the worker; closed when the stream ends.
	Packets <-chan VideoPacket
	// Shown is called with a frame's Seq once it is on the screen (the
	// worker's flow control).
	Shown func(seq uint64)
	// Input gets the viewer's mouse and keyboard (nil: view only).
	Input func(InputEvent)
	// NeedKeyframe asks the worker for a key frame (the decoder lost its state).
	NeedKeyframe func()
	// Status is shown in the title (bandwidth, latency), polled.
	Status func() string
}

// RunViewer opens the viewer window and runs it until the user closes it
// or Packets is closed. Call it from the main goroutine (some OSes need
// the main thread for windows).
func RunViewer(o ViewerOptions) error { return runViewer(o) }

// CanView: this OS has the viewer window (RunViewer).
func CanView() bool { return canView }

// Clipboard is this desktop's clipboard (text): the remote desktop copies
// and pastes both ways with it.
type Clipboard interface {
	// Changes delivers the text when somebody copies (not what Set put
	// there); closed when the clipboard is closed.
	Changes() <-chan string
	// Set puts text on the clipboard.
	Set(text string) error
	Close()
}

// OpenClipboard watches the clipboard of this process's desktop (one per
// process).
func OpenClipboard() (Clipboard, error) { return openClipboard() }
