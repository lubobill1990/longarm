package mobile

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"longarm/internal/rdesk/quality"
	pb "longarm/proto/longarm/v1"
)

// The desktop for the native viewer (docs/mobile.md "the remote desktop:
// native"): RemoteDesktop.Stream as `longarm desktop` uses it, with the
// frames handed to the app's decoder (VideoToolbox, MediaCodec) as they
// come - H.264 access units, Annex B - and its input sent on. The viewer
// acks each frame it showed (the worker's flow control); a session switch
// on the worker (its agent moves to another console session) is followed
// by a new stream.

// DesktopEvents is the viewer's side: called on the stream's goroutine.
type DesktopEvents interface {
	// Started: a new picture (size, codec "h264"): the next frame is a key
	// frame. displays: how many the worker has.
	Started(width, height int, codec string, displays int)
	Frame(data []byte, key bool, seq uint64)
	// Cursor moved or changed; shape (BGRA, w×h, hot spot) when it changed.
	Cursor(x, y int, visible bool, shapeID uint64, w, h, hotX, hotY int, bgra []byte)
	// Quality: the picture quality in force, after Started (the worker's
	// DesktopStarted: the preset asked for, auto's level now, the frame
	// rate and bitrate); preset "" from a worker without quality presets.
	Quality(preset, level string, fps, kbps int)
	// Note: something to show for a while ("UAC prompt…", a session switch).
	Note(text string)
	// Ended: the stream is over ("" when closed by Close).
	Ended(err string)
}

// DesktopOptions: what the viewer asks for.
type DesktopOptions struct {
	Display     int
	MaxFPS      int    // 0: 30
	BitrateKbps int    // 0: the worker's choice
	Quality     string // a preset (internal/rdesk/quality): "auto" follows the link
	ViewOnly    bool
}

// Desktop is an open desktop stream.
type Desktop struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	st     pb.RemoteDesktop_StreamClient
	closed atomic.Bool
	preset string // the quality asked for last (a new stream starts with it); under mu
}

// OpenDesktop opens a worker's desktop (one of the user's own).
func (c *Core) OpenDesktop(worker string, o DesktopOptions, ev DesktopEvents) (*Desktop, error) {
	cn, err := c.conn(context.Background(), worker)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if o.MaxFPS <= 0 {
		o.MaxFPS = 30
	}
	if err := quality.Valid(o.Quality); err != nil {
		cancel()
		return nil, err
	}
	d := &Desktop{ctx: ctx, cancel: cancel, preset: o.Quality}
	open := func() (pb.RemoteDesktop_StreamClient, error) {
		d.mu.Lock()
		start := &pb.DesktopStart{Display: int32(o.Display), Codecs: []string{"h264"}, MaxFps: int32(o.MaxFPS),
			BitrateKbps: int32(o.BitrateKbps), ViewOnly: o.ViewOnly}
		if d.preset != "" {
			start.Quality = &pb.DesktopQuality{Preset: d.preset}
		}
		d.mu.Unlock()
		st, err := cn.RemoteDesktop().Stream(ctx, grpc.WaitForReady(true))
		if err == nil {
			err = st.Send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Start{Start: start}})
		}
		return st, err
	}
	st, err := open()
	if err != nil {
		cancel()
		c.forget(worker, err)
		return nil, err
	}
	d.st = st
	go func() {
		var switching bool
		for {
			d.mu.Lock()
			cur := d.st
			d.mu.Unlock()
			m, err := cur.Recv()
			if err != nil && switching && ctx.Err() == nil {
				// the agent comes up in the new session: a new stream
				ok := false
				for i := 0; i < 30 && ctx.Err() == nil && !ok; i++ {
					time.Sleep(time.Second)
					if ns, err := open(); err == nil {
						d.mu.Lock()
						d.st = ns
						d.mu.Unlock()
						ok = true
					}
				}
				switching = false
				if ok {
					continue
				}
			}
			if err != nil {
				msg := ""
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					msg = err.Error()
					c.forget(worker, err)
				}
				ev.Ended(msg)
				return
			}
			switch v := m.Msg.(type) {
			case *pb.DesktopServerMessage_Started:
				s := v.Started
				ev.Started(int(s.Width), int(s.Height), s.Codec, len(s.Displays))
				ev.Quality(s.Quality, s.QualityLevel, int(s.Fps), int(s.BitrateKbps))
			case *pb.DesktopServerMessage_Frame:
				f := v.Frame
				if len(f.Data) > 0 {
					ev.Frame(f.Data, f.Key, f.Seq)
				} else {
					d.Ack(f.Seq) // a JPEG tile frame (not asked for): nothing to show
				}
			case *pb.DesktopServerMessage_Cursor:
				k := v.Cursor
				ev.Cursor(int(k.X), int(k.Y), k.Visible, k.ShapeId, int(k.Width), int(k.Height), int(k.HotX), int(k.HotY), k.Bgra)
			case *pb.DesktopServerMessage_State:
				s := v.State
				switch {
				case s.SwitchingSession:
					switching = true
					ev.Note(s.Note)
				case s.SecureHidden:
					ev.Note("the worker shows a secure screen (UAC prompt or lock screen) this connection can't see")
				case s.Note != "" && s.GetOptions() == nil:
					ev.Note(s.Note)
				}
			}
		}
	}()
	return d, nil
}

func (d *Desktop) send(m *pb.DesktopClientMessage) {
	if d.closed.Load() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.st.Send(m)
}

// Ack: the frame seq is on the screen.
func (d *Desktop) Ack(seq uint64) {
	d.send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Ack{Ack: seq}})
}

// Keyframe asks for a key frame (the decoder lost its state).
func (d *Desktop) Keyframe() {
	d.send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Keyframe{Keyframe: true}})
}

func (d *Desktop) input(in *pb.DesktopInput) {
	d.send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Input{Input: in}})
}

// Mouse: the pointer at x, y (the display's pixels); button 0 a move, 1
// left, 2 middle, 3 right, pressed or released.
func (d *Desktop) Mouse(x, y, button int, down bool) {
	d.input(&pb.DesktopInput{Event: &pb.DesktopInput_Mouse{Mouse: &pb.DesktopMouse{X: int32(x), Y: int32(y), Button: int32(button), Down: down}}})
}

// Wheel scrolls: notches × 120, positive up / right.
func (d *Desktop) Wheel(dx, dy int) {
	d.input(&pb.DesktopInput{Event: &pb.DesktopInput_Mouse{Mouse: &pb.DesktopMouse{WheelX: int32(dx), WheelY: int32(dy)}}})
}

// Key presses or releases a PC/AT set-1 scan code (0xE0xx: extended).
func (d *Desktop) Key(scan int, down bool) {
	d.input(&pb.DesktopInput{Event: &pb.DesktopInput_Key{Key: &pb.DesktopKey{Scan: uint32(scan), Down: down}}})
}

// Text types Unicode text (the soft keyboard, IMEs, paste).
func (d *Desktop) Text(s string) {
	d.input(&pb.DesktopInput{Event: &pb.DesktopInput_Text{Text: s}})
}

// SetQuality asks for another picture quality (a preset; the worker
// starts a new picture: Started, Quality).
func (d *Desktop) SetQuality(preset string) error {
	if err := quality.Valid(preset); err != nil {
		return err
	}
	d.mu.Lock()
	d.preset = preset
	d.mu.Unlock()
	d.send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Quality{Quality: &pb.DesktopQuality{Preset: preset}}})
	return nil
}

// SecureAttention sends Ctrl+Alt+Del.
func (d *Desktop) SecureAttention() {
	d.input(&pb.DesktopInput{Event: &pb.DesktopInput_SecureAttention{SecureAttention: true}})
}

// Close ends the stream (Ended("") follows).
func (d *Desktop) Close() {
	if d.closed.Swap(true) {
		return
	}
	d.cancel()
}
