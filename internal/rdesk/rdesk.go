// Package rdesk serves one viewer of Longarm's remote desktop
// (docs/remote-desktop.md): it captures a display (internal/screen),
// encodes it as H.264 or JPEG tiles, sends frames as fast as the viewer
// acknowledges them, and injects the viewer's input. longarmd's
// RemoteDesktop service runs it in-process; the Longarm Desktop agent runs
// it as SYSTEM in the console session. It knows nothing about who may
// connect: the caller decides that.
package rdesk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"longarm/internal/screen"
	pb "longarm/proto/longarm/v1"
)

// Stream is the worker's side of RemoteDesktop.Stream.
type Stream = interface {
	Context() context.Context
	Send(*pb.DesktopServerMessage) error
	Recv() (*pb.DesktopClientMessage, error)
}

// Codecs this worker can encode, best first.
func Codecs() []string { return []string{"h264", "jpeg"} }

const (
	window       = 3 // frames in flight before capture waits for an ack
	defaultFPS   = 30
	maxFPS       = 60
	jpegQuality  = 70
	refreshAfter = 150 * time.Millisecond // still that long: encode again, sharper
	refreshes    = 2
)

// Serve runs a viewer's session after its start message, until the
// stream ends.
func Serve(st Stream, start *pb.DesktopStart, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(st.Context())
	defer cancel()
	s := &session{st: st, start: start, log: log, ackCh: make(chan struct{}, 1), kfCh: make(chan struct{}, 1)}
	s.fps = int(start.GetMaxFps())
	if s.fps <= 0 {
		s.fps = defaultFPS
	}
	s.fps = min(s.fps, maxFPS)
	if !start.GetViewOnly() {
		in, err := screen.NewInjector()
		if err != nil {
			return fmt.Errorf("input: %w", err)
		}
		defer in.Close()
		s.in = in
	}
	// The clipboard both ways (where this OS has one): what is copied here
	// goes to the viewer.
	if cb, err := screen.OpenClipboard(); err == nil {
		defer cb.Close()
		s.clip = cb
		go func() {
			for text := range cb.Changes() {
				if ctx.Err() != nil {
					return
				}
				s.send(&pb.DesktopServerMessage{Msg: &pb.DesktopServerMessage_Clipboard{Clipboard: text}})
			}
		}()
	}
	go func() { s.recv(ctx); cancel() }()
	err := s.capture(ctx)
	if ctx.Err() != nil {
		return nil // the viewer went (or another took over): that is how sessions end
	}
	return err
}

type session struct {
	st    Stream
	start *pb.DesktopStart
	log   *slog.Logger
	in    screen.Injector
	fps   int

	sent, acked atomic.Uint64
	ackCh       chan struct{}
	kfCh        chan struct{}

	mu     sync.Mutex
	origin [2]int // the display's desktop coordinates (input is relative to it)

	sendMu sync.Mutex       // frames and clipboard text come from two goroutines
	clip   screen.Clipboard // nil: no clipboard here
}

// recv takes the viewer's acks, keyframe requests and input.
func (s *session) recv(ctx context.Context) error {
	for {
		m, err := s.st.Recv()
		if err != nil {
			return err
		}
		switch v := m.Msg.(type) {
		case *pb.DesktopClientMessage_Ack:
			if v.Ack > s.acked.Load() {
				s.acked.Store(v.Ack)
			}
			signal(s.ackCh)
		case *pb.DesktopClientMessage_Clipboard:
			// The viewer copied: paste-able here (not when only watching).
			if s.clip != nil && !s.start.GetViewOnly() && len(v.Clipboard) <= screen.MaxClipboardText {
				if err := s.clip.Set(v.Clipboard); err != nil {
					s.log.Debug("remote desktop: clipboard", "err", err)
				}
			}
		case *pb.DesktopClientMessage_Keyframe:
			signal(s.kfCh)
		case *pb.DesktopClientMessage_Input:
			if s.in != nil {
				if err := s.input(v.Input); err != nil {
					s.log.Debug("remote desktop: input", "err", err)
				}
			}
		}
	}
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (s *session) input(e *pb.DesktopInput) error {
	s.mu.Lock()
	ox, oy := s.origin[0], s.origin[1]
	s.mu.Unlock()
	switch v := e.Event.(type) {
	case *pb.DesktopInput_Mouse:
		m := v.Mouse
		if m.WheelX != 0 || m.WheelY != 0 {
			return s.in.Wheel(int(m.WheelX), int(m.WheelY))
		}
		if err := s.in.Move(ox+int(m.X), oy+int(m.Y)); err != nil {
			return err
		}
		if m.Button != 0 {
			return s.in.Button(int(m.Button), m.Down)
		}
	case *pb.DesktopInput_Key:
		return s.in.Key(uint16(v.Key.Scan), v.Key.Down)
	case *pb.DesktopInput_Text:
		return s.in.Text(v.Text)
	case *pb.DesktopInput_SecureAttention:
		return s.in.SecureAttention()
	}
	return nil
}

// pipeline is what turns a source's frames into messages.
type pipeline struct {
	src    screen.Source
	w, h   int // even
	codec  string
	enc    screen.VideoEncoder
	prev   []byte // jpeg: the last frame sent
	crop   []byte
	sentAt time.Time
}

func (p *pipeline) close() {
	if p.enc != nil {
		p.enc.Close()
	}
	if p.src != nil {
		p.src.Close()
	}
}

func (s *session) open() (*pipeline, error) {
	src, err := screen.OpenSource(int(s.start.GetDisplay()))
	if err != nil {
		return nil, err
	}
	p := &pipeline{src: src}
	w, h := src.Size()
	p.w, p.h = w&^1, h&^1
	want := s.start.GetCodecs()
	if len(want) == 0 {
		want = Codecs()
	}
	var encErr error
	for _, c := range want {
		switch c {
		case "h264":
			bitrate := int(s.start.GetBitrateKbps()) * 1000
			if bitrate <= 0 {
				bitrate = min(max(p.w*p.h*2, 2_000_000), 20_000_000)
			}
			p.enc, encErr = screen.NewH264Encoder(screen.EncoderOptions{Width: p.w, Height: p.h, FPS: s.fps, Bitrate: bitrate})
			if encErr == nil {
				p.codec = "h264"
			}
		case "jpeg":
			p.codec = "jpeg"
		}
		if p.codec != "" {
			break
		}
	}
	if p.codec == "" {
		src.Close()
		if encErr != nil {
			return nil, fmt.Errorf("no codec the viewer takes (%v): %w", want, encErr)
		}
		return nil, fmt.Errorf("no codec the viewer takes (%v); this worker encodes %v", want, Codecs())
	}
	d := src.Display()
	s.mu.Lock()
	s.origin = [2]int{d.X, d.Y}
	s.mu.Unlock()
	encoder := "jpeg"
	if p.enc != nil {
		encoder = p.enc.Name()
	}
	return p, s.send(&pb.DesktopServerMessage{Msg: &pb.DesktopServerMessage_Started{Started: &pb.DesktopStarted{
		Width: int32(p.w), Height: int32(p.h), Codec: p.codec, Encoder: encoder,
		Display: &pb.DesktopDisplay{Index: int32(d.Index), Name: d.Name, X: int32(d.X), Y: int32(d.Y), Width: int32(d.W), Height: int32(d.H)},
	}}})
}

// capture is the session's main loop (one goroutine: sources and
// encoders are tied to their thread on some OSes).
func (s *session) capture(ctx context.Context) error {
	var p *pipeline
	defer func() {
		if p != nil {
			p.close()
		}
	}()
	interval := time.Second / time.Duration(s.fps)
	hidden := false
	var lastCursor screen.Cursor
	refresh := 0
	for ctx.Err() == nil {
		if p == nil {
			var err error
			if p, err = s.open(); err != nil {
				if errors.Is(err, screen.ErrUnsupported) {
					return err
				}
				// A secure screen this process may not capture (or the
				// display is changing): say so, and try again.
				if !hidden {
					hidden = true
					s.log.Debug("remote desktop: cannot capture", "err", err)
					if err := s.send(&pb.DesktopServerMessage{Msg: &pb.DesktopServerMessage_State{State: &pb.DesktopState{SecureHidden: true, Note: err.Error()}}}); err != nil {
						return err
					}
				}
				select {
				case <-ctx.Done():
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			if hidden {
				hidden = false
				if err := s.send(&pb.DesktopServerMessage{Msg: &pb.DesktopServerMessage_State{State: &pb.DesktopState{}}}); err != nil {
					return err
				}
			}
			lastCursor = screen.Cursor{}
			refresh = 0
		}
		// Flow control: wait for the viewer.
		for s.sent.Load()-s.acked.Load() >= window && ctx.Err() == nil {
			select {
			case <-s.ackCh:
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				// No ack for long: the viewer may have lost them (it acks
				// the newest it showed); carry on.
				s.acked.Store(s.sent.Load())
			}
		}
		if wait := interval - time.Since(p.sentAt); wait > 0 {
			time.Sleep(wait) // pace before capturing, so the frame is fresh
		}
		timeout := 100 * time.Millisecond
		if refresh > 0 {
			timeout = refreshAfter
		}
		changed, err := p.src.Next(timeout)
		if err != nil {
			p.close()
			p = nil
			continue
		}
		if err := s.sendCursor(p, &lastCursor); err != nil {
			return err
		}
		keyframe := false
		select {
		case <-s.kfCh:
			keyframe = true
		default:
		}
		switch {
		case changed:
			refresh = refreshes
		case keyframe:
		case refresh > 0 && p.codec == "h264":
			refresh--
		default:
			continue
		}
		if err := s.sendFrame(p, keyframe); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) sendCursor(p *pipeline, last *screen.Cursor) error {
	c := p.src.Cursor()
	if c.X == last.X && c.Y == last.Y && c.Visible == last.Visible && c.ShapeID == last.ShapeID {
		return nil
	}
	m := &pb.DesktopCursor{X: int32(c.X), Y: int32(c.Y), Visible: c.Visible, ShapeId: c.ShapeID}
	if c.ShapeID != last.ShapeID && c.BGRA != nil {
		m.Width, m.Height, m.HotX, m.HotY, m.Bgra = int32(c.W), int32(c.H), int32(c.HotX), int32(c.HotY), c.BGRA
	}
	*last = c
	last.BGRA = nil
	return s.send(&pb.DesktopServerMessage{Msg: &pb.DesktopServerMessage_Cursor{Cursor: m}})
}

func (s *session) sendFrame(p *pipeline, keyframe bool) error {
	captured := time.Now()
	frame := p.src.Frame()
	if w, _ := p.src.Size(); w != p.w {
		// Odd width: drop the last column (H.264 wants even sizes).
		if p.crop == nil {
			p.crop = make([]byte, p.w*p.h*4)
		}
		for y := 0; y < p.h; y++ {
			copy(p.crop[y*p.w*4:(y+1)*p.w*4], frame[y*w*4:])
		}
		frame = p.crop
	}
	f := &pb.DesktopFrame{Captured: timestamppb.New(captured)}
	switch p.codec {
	case "h264":
		if keyframe {
			p.enc.ForceKeyframe()
		}
		au, err := p.enc.Encode(frame[:p.w*p.h*4])
		if err != nil {
			return fmt.Errorf("encode: %w", err)
		}
		if len(au) == 0 {
			return nil
		}
		f.Data, f.Key = au, isIDR(au)
	case "jpeg":
		if keyframe || p.prev == nil {
			p.prev = nil
			f.Key = true
		}
		var rects []screen.Rect
		if p.prev == nil {
			p.prev = slices.Clone(frame[:p.w*p.h*4])
			rects = []screen.Rect{{W: p.w, H: p.h}}
		} else {
			rects = screen.ChangedTiles(frame[:p.w*p.h*4], p.prev, p.w, p.h)
		}
		if len(rects) == 0 {
			return nil
		}
		for _, t := range screen.EncodeTiles(frame, p.w, rects, jpegQuality) {
			f.Tiles = append(f.Tiles, &pb.DesktopTile{X: int32(t.X), Y: int32(t.Y), Width: int32(t.W), Height: int32(t.H), Jpeg: t.JPEG})
		}
	}
	f.Seq = s.sent.Add(1)
	p.sentAt = time.Now()
	return s.send(&pb.DesktopServerMessage{Msg: &pb.DesktopServerMessage_Frame{Frame: f}})
}

// isIDR: the access unit holds an IDR picture (NAL type 5).
func isIDR(au []byte) bool {
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 && au[i+3]&0x1F == 5 {
			return true
		}
	}
	return false
}

// Host is this process's desktop for longarmd's RemoteDesktop service
// (server.DesktopHost).
type Host struct{ Log *slog.Logger }

func (Host) Available() string { return screen.Available() }

// Displays lists the displays and the codecs (DesktopDisplaysResponse).
func (Host) Displays() *pb.DesktopDisplaysResponse {
	out := &pb.DesktopDisplaysResponse{NativeReason: screen.Available(), Codecs: Codecs()}
	if out.NativeReason != "" {
		return out
	}
	ds, err := screen.Displays()
	if err != nil {
		out.NativeReason = err.Error()
	}
	for _, d := range ds {
		out.Displays = append(out.Displays, &pb.DesktopDisplay{Index: int32(d.Index), Name: d.Name,
			X: int32(d.X), Y: int32(d.Y), Width: int32(d.W), Height: int32(d.H)})
	}
	return out
}

// Serve is Serve with gRPC status errors.
func (h Host) Serve(st Stream, start *pb.DesktopStart) error {
	err := Serve(st, start, h.Log)
	if errors.Is(err, screen.ErrUnsupported) {
		return status.Error(codes.Unimplemented, err.Error())
	}
	return err
}

func (s *session) send(m *pb.DesktopServerMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.st.Send(m)
}
