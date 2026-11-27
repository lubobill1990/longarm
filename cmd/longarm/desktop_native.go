package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"longarm/internal/client"
	"longarm/internal/screen"
	pb "longarm/proto/longarm/v1"
)

// nativeOpts: `longarm desktop` with Longarm's own remote desktop
// (docs/remote-desktop.md).
type nativeOpts struct {
	display    int
	viewOnly   bool
	codec      string // "" = h264, else jpeg
	screenshot string // save the screen as this PNG and exit (no window)
	input      string // --input: events to send first (tests, scripts)
	fps        int
}

// canViewNatively: this client can open the viewer window.
func canViewNatively() bool { return screen.CanView() }

func hasFeature(info *pb.InfoResponse, f string) bool { return slices.Contains(info.GetFeatures(), f) }

// nativeDesktop shows worker's desktop in a window (or takes a screenshot).
func nativeDesktop(ctx context.Context, conn *client.Conn, worker string, o nativeOpts) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := conn.RemoteDesktop().Stream(ctx, grpc.WaitForReady(true))
	if err != nil {
		return err
	}
	codecs := []string{"h264", "jpeg"}
	if o.codec != "" {
		codecs = []string{o.codec}
	}
	fps := o.fps
	if fps <= 0 {
		fps = 60
	}
	var sendMu sync.Mutex
	send := func(m *pb.DesktopClientMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return st.Send(m)
	}
	if err := send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Start{Start: &pb.DesktopStart{
		Display: int32(o.display), Codecs: codecs, MaxFps: int32(fps), ViewOnly: o.viewOnly}}}); err != nil {
		return err
	}
	events, err := parseInputScript(o.input)
	if err != nil {
		return usagef("--input: %v", err)
	}

	// The clipboard both ways in the window (not for a screenshot): what
	// is copied here goes to the worker unless only watching.
	var clip screen.Clipboard
	if o.screenshot == "" {
		if c, err := screen.OpenClipboard(); err == nil {
			clip = c
			defer c.Close()
			if !o.viewOnly {
				go func() {
					for text := range c.Changes() {
						if ctx.Err() != nil {
							return
						}
						send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Clipboard{Clipboard: text}})
					}
				}()
			}
		}
	}
	packets := make(chan screen.VideoPacket, 8)
	var received atomic.Int64
	var frames atomic.Int64
	var info atomic.Value // string: codec, encoder, size
	recvErr := make(chan error, 1)
	go func() {
		defer close(packets)
		for {
			m, err := st.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) || ctx.Err() != nil {
					err = nil
				}
				recvErr <- err
				return
			}
			received.Add(int64(len(m.String()))) // close enough; frames dominate
			var p screen.VideoPacket
			switch v := m.Msg.(type) {
			case *pb.DesktopServerMessage_Started:
				s := v.Started
				p = screen.VideoPacket{Config: true, Width: int(s.Width), Height: int(s.Height), Codec: s.Codec}
				info.Store(fmt.Sprintf("%s (%s) %dx%d", s.Codec, s.Encoder, s.Width, s.Height))
			case *pb.DesktopServerMessage_Frame:
				f := v.Frame
				received.Add(int64(len(f.Data)))
				frames.Add(1)
				p = screen.VideoPacket{Data: f.Data, Key: f.Key, Seq: f.Seq, Captured: f.Captured.AsTime()}
				for _, t := range f.Tiles {
					received.Add(int64(len(t.Jpeg)))
					p.Tiles = append(p.Tiles, screen.Tile{X: int(t.X), Y: int(t.Y), W: int(t.Width), H: int(t.Height), JPEG: t.Jpeg})
				}
			case *pb.DesktopServerMessage_Cursor:
				c := v.Cursor
				p.Cursor = &screen.Cursor{X: int(c.X), Y: int(c.Y), Visible: c.Visible, ShapeID: c.ShapeId,
					W: int(c.Width), H: int(c.Height), HotX: int(c.HotX), HotY: int(c.HotY), BGRA: c.Bgra}
			case *pb.DesktopServerMessage_Clipboard:
				// Copied on the worker: paste-able here.
				if clip != nil {
					if err := clip.Set(v.Clipboard); err != nil {
						fmt.Fprintf(os.Stderr, "longarm: clipboard: %v\n", err)
					}
				}
				continue
			case *pb.DesktopServerMessage_State:
				p.SecureHidden, p.SecureHiddenOK = v.State.SecureHidden, true
				if v.State.SecureHidden {
					fmt.Fprintf(os.Stderr, "longarm: %s shows a secure screen (UAC prompt or lock screen) that cannot be captured without the Longarm Desktop service\n", worker)
				}
			default:
				continue
			}
			select {
			case packets <- p:
			case <-ctx.Done():
				return
			}
		}
	}()
	ack := func(seq uint64) { send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Ack{Ack: seq}}) }
	input := func(e screen.InputEvent) {
		if m := inputMessage(e); m != nil {
			send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Input{Input: m}})
		}
	}

	if o.screenshot != "" {
		keyframe := func() { send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Keyframe{Keyframe: true}}) }
		err := screenshot(ctx, packets, recvErr, ack, keyframe, input, events, o.screenshot)
		cancel()
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "longarm: saved %s's screen to %s\n", worker, o.screenshot)
		return nil
	}
	for _, e := range events {
		input(e)
	}
	start := time.Now()
	var lastBytes, lastFrames int64
	var lastAt = start
	vo := screen.ViewerOptions{
		Title:   "Longarm - " + worker,
		Packets: packets,
		Shown:   ack,
		NeedKeyframe: func() {
			send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Keyframe{Keyframe: true}})
		},
		Status: func() string {
			now := time.Now()
			s := now.Sub(lastAt).Seconds()
			b, f := received.Load(), frames.Load()
			out := ""
			if v, ok := info.Load().(string); ok {
				out = v + " - "
			}
			out += fmt.Sprintf("%.0f fps, %.2f Mbit/s", float64(f-lastFrames)/s, float64(b-lastBytes)*8/1e6/s)
			lastBytes, lastFrames, lastAt = b, f, now
			return out
		},
	}
	if !o.viewOnly {
		vo.Input = input
	}
	verr := screen.RunViewer(vo)
	cancel()
	if verr != nil {
		return verr
	}
	select {
	case err := <-recvErr:
		return err
	default:
		return nil
	}
}

// screenshot decodes the first full picture (after events, when given)
// and saves it as a PNG.
func screenshot(ctx context.Context, packets <-chan screen.VideoPacket, recvErr <-chan error, ack func(uint64), keyframe func(), input func(screen.InputEvent), events []screen.InputEvent, path string) error {
	var cfg screen.VideoPacket
	var dec screen.VideoDecoder
	defer func() {
		if dec != nil {
			dec.Close()
		}
	}()
	var img []byte
	sentInput := len(events) == 0
	var after time.Time // the picture to save is the first one decoded after this
	deadline := time.After(60 * time.Second)
	for {
		var p screen.VideoPacket
		var ok bool
		select {
		case p, ok = <-packets:
		case <-deadline:
			return errors.New("no picture from the worker in 60 s")
		case <-ctx.Done():
			return ctx.Err()
		}
		if !ok {
			select {
			case err := <-recvErr:
				if err != nil {
					return err // the stream's error (refused, unavailable) came before the close
				}
			default:
			}
			return errors.New("the worker ended the stream before a picture came")
		}
		switch {
		case p.Config:
			cfg = p
			img = make([]byte, p.Width*p.Height*4)
			if dec != nil {
				dec.Close()
				dec = nil
			}
			if p.Codec == "h264" {
				var err error
				if dec, err = screen.NewH264Decoder(p.Width, p.Height); err != nil {
					return fmt.Errorf("H.264 decoder: %w", err)
				}
			}
			continue
		case p.Cursor != nil || p.SecureHiddenOK:
			continue
		}
		if img == nil {
			continue
		}
		got := false
		switch cfg.Codec {
		case "h264":
			out, _, _, err := dec.Decode(p.Data)
			if err != nil {
				return fmt.Errorf("decode: %w", err)
			}
			if out != nil {
				copy(img, out)
				got = true
			}
		case "jpeg":
			screen.DrawTiles(img, cfg.Width, cfg.Height, p.Tiles)
			got = true
		}
		ack(p.Seq)
		if !got {
			continue
		}
		if !sentInput {
			// The screen is up: type and click, then wait for the result.
			for _, e := range events {
				input(e)
			}
			sentInput, after = true, time.Now().Add(time.Second)
			// A still screen sends nothing: ask for a fresh picture then.
			time.AfterFunc(1100*time.Millisecond, keyframe)
			continue
		}
		if time.Now().Before(after) {
			continue
		}
		return savePNG(path, img, cfg.Width, cfg.Height)
	}
}

func savePNG(path string, bgra []byte, w, h int) error {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i+3 < len(bgra) && i+3 < len(m.Pix); i += 4 {
		m.Pix[i], m.Pix[i+1], m.Pix[i+2], m.Pix[i+3] = bgra[i+2], bgra[i+1], bgra[i], 255
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, m); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func inputMessage(e screen.InputEvent) *pb.DesktopInput {
	switch {
	case e.Mouse != nil:
		m := e.Mouse
		return &pb.DesktopInput{Event: &pb.DesktopInput_Mouse{Mouse: &pb.DesktopMouse{X: int32(m.X), Y: int32(m.Y),
			Button: int32(m.Button), Down: m.Down, WheelX: int32(m.WheelX), WheelY: int32(m.WheelY)}}}
	case e.Key != nil:
		return &pb.DesktopInput{Event: &pb.DesktopInput_Key{Key: &pb.DesktopKey{Scan: uint32(e.Key.Scan), Down: e.Key.Down}}}
	case e.Text != "":
		return &pb.DesktopInput{Event: &pb.DesktopInput_Text{Text: e.Text}}
	case e.SAS:
		return &pb.DesktopInput{Event: &pb.DesktopInput_SecureAttention{SecureAttention: true}}
	}
	return nil
}

// parseInputScript reads --input: events separated by ";":
//
//	click:X,Y        left click at the remote display's X,Y
//	move:X,Y
//	key:SCAN[,SCAN]  press the scan codes in order, release them in reverse
//	text:TEXT        type Unicode text
//	sas              Ctrl+Alt+Del
func parseInputScript(s string) ([]screen.InputEvent, error) {
	var out []screen.InputEvent
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kind, arg, _ := strings.Cut(part, ":")
		switch kind {
		case "click", "move":
			var x, y int
			if _, err := fmt.Sscanf(arg, "%d,%d", &x, &y); err != nil {
				return nil, fmt.Errorf("%s: want X,Y", part)
			}
			out = append(out, screen.InputEvent{Mouse: &screen.MouseEvent{X: x, Y: y}})
			if kind == "click" {
				out = append(out,
					screen.InputEvent{Mouse: &screen.MouseEvent{X: x, Y: y, Button: screen.ButtonLeft, Down: true}},
					screen.InputEvent{Mouse: &screen.MouseEvent{X: x, Y: y, Button: screen.ButtonLeft}})
			}
		case "key":
			var scans []uint16
			for _, f := range strings.Split(arg, ",") {
				v, err := strconv.ParseUint(strings.TrimSpace(f), 0, 16)
				if err != nil {
					return nil, fmt.Errorf("%s: %v", part, err)
				}
				scans = append(scans, uint16(v))
			}
			for _, c := range scans {
				out = append(out, screen.InputEvent{Key: &screen.KeyEvent{Scan: c, Down: true}})
			}
			for i := len(scans) - 1; i >= 0; i-- {
				out = append(out, screen.InputEvent{Key: &screen.KeyEvent{Scan: scans[i]}})
			}
		case "text":
			out = append(out, screen.InputEvent{Text: arg})
		case "sas":
			out = append(out, screen.InputEvent{SAS: true})
		default:
			return nil, fmt.Errorf("unknown event %q", part)
		}
	}
	return out, nil
}
