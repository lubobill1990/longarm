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
	"runtime"
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

// SwitchSessions: this process may move Windows sessions to the console
// (the Longarm Desktop service's agent, SYSTEM on the console): the
// viewer's session menu, and on connect the one disconnected session when
// nobody is signed in at the console (screen.ReattachCandidate).
var SwitchSessions bool

// Serve runs a viewer's session after its start message, until the
// stream ends.
func Serve(st Stream, start *pb.DesktopStart, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(st.Context())
	defer cancel()
	s := &session{st: st, start: start, log: log, ackCh: make(chan struct{}, 1), kfCh: make(chan struct{}, 1)}
	// Nobody signed in at the console and one session of the machine's
	// waiting disconnected (its Remote Desktop window was closed): show
	// that one instead of the sign-in screen.
	if SwitchSessions && !start.GetViewOnly() {
		if ss, err := screen.Sessions(); err == nil {
			if c, ok := screen.ReattachCandidate(ss); ok {
				if s.switchSession(c.ID, c.User) {
					return nil
				}
			}
		}
	}
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
	s.display.Store(start.GetDisplay())
	s.switchCh = make(chan struct{}, 1)
	// Speakers to capture (a machine without any - a Cloud PC nobody is
	// connected to over RDP, say - offers no sound).
	if c, err := screen.OpenAudioCapture(); err == nil {
		c.Close()
		s.canSound = true
	}
	s.canMic = screen.MicFeedAvailable()
	defer s.leave()
	if o := start.GetOptions(); o != nil {
		s.setOptions(o)
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

	display  atomic.Int32  // the display being shown
	switchCh chan struct{} // switch_display: the capture loop opens it

	optMu       sync.Mutex
	curtain     screen.Curtain // privacy mode on
	lockOnLeave bool
	sound       screen.AudioCapture // sound on: this machine's sound to the viewer
	canSound    bool                // there are speakers to capture
	mic         bool                // microphone on: the viewer's goes to micFeed
	micFeed     screen.AudioPlayer  // opened with the first packet's format
	micFormat   screen.AudioFormat
	canMic      bool // the Longarm audio driver is here (screen.MicFeedAvailable)
}

// micPacket plays a packet of the viewer's microphone into the Longarm
// audio driver (programs here record it from "Longarm Microphone").
func (s *session) micPacket(a *pb.DesktopAudio) {
	if a.Codec != screen.AudioCodec {
		return
	}
	f := screen.AudioFormat{Rate: int(a.Rate), Channels: int(a.Channels)}
	pcm, err := screen.DecodeADPCM(a.Data, f.Channels)
	if err != nil {
		return
	}
	s.optMu.Lock()
	defer s.optMu.Unlock()
	if !s.mic {
		return
	}
	if s.micFeed == nil || f != s.micFormat {
		if s.micFeed != nil {
			s.micFeed.Close()
			s.micFeed = nil
		}
		p, err := screen.OpenMicFeed(f)
		if err != nil {
			s.log.Info("remote desktop: microphone", "err", err)
			s.mic = false // say so below, once
			go s.setOptions(&pb.DesktopOptions{})
			return
		}
		s.micFeed, s.micFormat = p, f
	}
	s.micFeed.Write(pcm)
}

// startSound sends what this machine's speakers play until the capture is
// closed (sound off, the viewer gone). Under optMu.
func (s *session) startSound() error {
	c, err := screen.OpenAudioCapture()
	if err != nil {
		return err
	}
	s.sound = c
	go func() {
		f := c.Format()
		enc := screen.NewADPCMEncoder(f.Channels)
		var seq uint64
		for {
			pcm, err := c.Read()
			if err != nil {
				return // closed
			}
			seq++
			if err := s.send(&pb.DesktopServerMessage{Msg: &pb.DesktopServerMessage_Sound{Sound: &pb.DesktopAudio{
				Codec: screen.AudioCodec, Rate: int32(f.Rate), Channels: int32(f.Channels), Data: enc.Encode(pcm), Seq: seq}}}); err != nil {
				return
			}
		}
	}()
	return nil
}

// setOptions applies what the viewer switched (unset fields stay) and tells
// it what is in force.
func (s *session) setOptions(o *pb.DesktopOptions) {
	s.optMu.Lock()
	if o.Privacy != nil && !s.start.GetViewOnly() {
		switch {
		case *o.Privacy && s.curtain == nil:
			c, err := screen.NewCurtain("This computer is being used remotely (Longarm)")
			if err != nil {
				s.log.Info("remote desktop: privacy mode", "err", err)
			} else {
				s.curtain = c
			}
		case !*o.Privacy && s.curtain != nil:
			s.curtain.Close()
			s.curtain = nil
		}
	}
	if o.LockOnLeave != nil {
		s.lockOnLeave = *o.LockOnLeave
	}
	if o.Sound != nil {
		switch {
		case *o.Sound && s.sound == nil && s.canSound:
			if err := s.startSound(); err != nil {
				s.log.Info("remote desktop: sound", "err", err)
			}
		case !*o.Sound && s.sound != nil:
			s.sound.Close()
			s.sound = nil
		}
	}
	if o.Microphone != nil && !s.start.GetViewOnly() {
		s.mic = *o.Microphone && s.canMic
		if !s.mic && s.micFeed != nil {
			s.micFeed.Close()
			s.micFeed = nil
		}
	}
	privacy, lock, sound, mic := s.curtain != nil, s.lockOnLeave, s.sound != nil, s.mic
	s.optMu.Unlock()
	s.send(&pb.DesktopServerMessage{Msg: &pb.DesktopServerMessage_State{State: &pb.DesktopState{
		Options: &pb.DesktopOptions{Privacy: &privacy, LockOnLeave: &lock, Sound: &sound, Microphone: &mic}}}})
}

// leave: the viewer went - privacy mode ends, the session locks if asked.
func (s *session) leave() {
	s.optMu.Lock()
	defer s.optMu.Unlock()
	if s.curtain != nil {
		s.curtain.Close()
		s.curtain = nil
	}
	if s.sound != nil {
		s.sound.Close()
		s.sound = nil
	}
	if s.micFeed != nil {
		s.micFeed.Close()
		s.micFeed = nil
	}
	if s.lockOnLeave {
		if err := screen.LockSession(); err != nil {
			s.log.Info("remote desktop: lock on leave", "err", err)
		}
	}
}

// switchSession moves session id (user's) to the console and tells the
// viewer, which connects again: the Longarm Desktop service moves its
// agent - this process - to the new console session. false: it did not
// move (said in the state's note).
func (s *session) switchSession(id uint32, user string) bool {
	who := user
	if who == "" {
		who = fmt.Sprintf("session %d", id)
	}
	if err := screen.ConnectToConsole(id); err != nil {
		s.log.Info("remote desktop: switch session", "session", id, "err", err)
		s.send(&pb.DesktopServerMessage{Msg: &pb.DesktopServerMessage_State{State: &pb.DesktopState{
			Note: fmt.Sprintf("could not show %s: %v", who, err)}}})
		return false
	}
	s.log.Info("remote desktop: moved a session to the console", "session", id, "user", user)
	s.send(&pb.DesktopServerMessage{Msg: &pb.DesktopServerMessage_State{State: &pb.DesktopState{
		SwitchingSession: true, Note: "showing " + who}}})
	// The service ends this agent once it sees the console's new session;
	// until then there is nothing to show.
	for i := 0; i < 40 && screen.ConsoleSession() != id; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// sessionUser: who is signed in to session id ("" = nobody or not known).
func sessionUser(id uint32) string {
	ss, _ := screen.Sessions()
	for _, x := range ss {
		if x.ID == id {
			return x.User
		}
	}
	return ""
}

// sessionsPB: the machine's sessions for DesktopStarted (can "sessions").
func sessionsPB() []*pb.DesktopSession {
	if !SwitchSessions {
		return nil
	}
	ss, err := screen.Sessions()
	if err != nil {
		return nil
	}
	out := make([]*pb.DesktopSession, 0, len(ss))
	for _, x := range ss {
		out = append(out, &pb.DesktopSession{Id: x.ID, User: x.User, State: x.State, Station: x.Station, Console: x.Console})
	}
	return out
}

// can: what this worker offers (DesktopStarted.can).
func (s *session) can() []string {
	var out []string
	if runtime.GOOS == "windows" { // elsewhere: todo/remote-desktop.md
		out = append(out, "privacy", "lock_on_leave")
	}
	if s.clip != nil {
		out = append(out, "clipboard")
	}
	if SwitchSessions && !s.start.GetViewOnly() {
		out = append(out, "sessions")
	}
	if s.canSound {
		out = append(out, "sound")
	}
	// Your microphone here: only with the Longarm audio driver installed
	// (it needs Microsoft's signature to load; without it no microphone).
	if s.canMic && !s.start.GetViewOnly() {
		out = append(out, "microphone")
	}
	return out
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
		case *pb.DesktopClientMessage_Options:
			s.setOptions(v.Options)
		case *pb.DesktopClientMessage_Microphone:
			s.micPacket(v.Microphone)
		case *pb.DesktopClientMessage_SwitchSession:
			// the session menu: this stream ends once it moved (the viewer connects again)
			if SwitchSessions && !s.start.GetViewOnly() && s.switchSession(v.SwitchSession, sessionUser(v.SwitchSession)) {
				return nil
			}
		case *pb.DesktopClientMessage_SwitchDisplay:
			s.display.Store(v.SwitchDisplay)
			signal(s.switchCh)
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
	src, err := screen.OpenSource(int(s.display.Load()))
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
		Displays: displays(), Can: s.can(), Sessions: sessionsPB(),
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
		select {
		case <-s.switchCh: // another display: open it (a new started, a key frame)
			p.close()
			p = nil
			continue
		default:
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

// displays: the displays for DesktopStarted (a viewer's picker).
func displays() []*pb.DesktopDisplay {
	ds, _ := screen.Displays()
	out := make([]*pb.DesktopDisplay, 0, len(ds))
	for _, d := range ds {
		out = append(out, &pb.DesktopDisplay{Index: int32(d.Index), Name: d.Name, X: int32(d.X), Y: int32(d.Y), Width: int32(d.W), Height: int32(d.H)})
	}
	return out
}
