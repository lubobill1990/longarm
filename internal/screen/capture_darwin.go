//go:build darwin

package screen

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

// macDisplay is a Display with what Quartz calls it: its ID, its bounds
// in points (the global coordinate space, origin at the top left of the
// main display) and its pixels per point.
type macDisplay struct {
	Display
	id    uint32
	pts   cgRect
	scale float64
}

// macDisplays lists the active displays, the main one first. A display's
// desktop coordinates are its point origin times its scale, so pixels on
// one display are pixels of the Display (mixed scales leave gaps between
// displays, which Move handles).
func macDisplays() ([]macDisplay, error) {
	if err := macLoad(); err != nil {
		return nil, err
	}
	var ids [32]uint32
	var n uint32
	if r := cgGetActiveDisplayList(uint32(len(ids)), &ids[0], &n); r != 0 {
		return nil, fmt.Errorf("CGGetActiveDisplayList: error %d", r)
	}
	list := ids[:n]
	mainID := cgMainDisplayID()
	for i, id := range list {
		if id == mainID && i > 0 {
			list[0], list[i] = list[i], list[0]
		}
	}
	var out []macDisplay
	for i, id := range list {
		b := cgDisplayBounds(id)
		pw, ph := int(b.Size.W), int(b.Size.H)
		if m := cgDisplayCopyDisplayMode(id); m != 0 {
			pw, ph = int(cgDisplayModeGetPixelWidth(m)), int(cgDisplayModeGetPixelHeight(m))
			cgDisplayModeRelease(m)
		}
		scale := 1.0
		if b.Size.W > 0 {
			scale = float64(pw) / b.Size.W
		}
		out = append(out, macDisplay{
			Display: Display{Index: i, Name: fmt.Sprintf("display %d", id),
				X: int(math.Round(b.Origin.X * scale)), Y: int(math.Round(b.Origin.Y * scale)), W: pw, H: ph},
			id: id, pts: b, scale: scale,
		})
	}
	return out, nil
}

// Displays lists the monitors, the main one first.
func Displays() ([]Display, error) {
	ds, err := macDisplays()
	if err != nil {
		return nil, err
	}
	out := make([]Display, len(ds))
	for i, d := range ds {
		out[i] = d.Display
	}
	return out, nil
}

var (
	sckOnce       sync.Once
	sckErr        error
	scFrameStatus uintptr // SCStreamFrameInfoStatus
	scOutputClass objc.Class
	scSources     sync.Map // the output object (objc.ID) → *macSource
)

// sckLoad loads ScreenCaptureKit (macOS 12.3+) and registers the stream
// output class.
func sckLoad() error {
	sckOnce.Do(func() {
		if sckErr = macLoad(); sckErr != nil {
			return
		}
		l := framework("ScreenCaptureKit")
		scFrameStatus = l.obj("SCStreamFrameInfoStatus")
		if l.err != nil || class("SCStream") == 0 || class("SCShareableContent") == 0 {
			sckErr = errors.New("ScreenCaptureKit is missing (macOS 12.3 or later is needed)")
			return
		}
		var protocols []*objc.Protocol
		for _, name := range []string{"SCStreamOutput", "SCStreamDelegate"} {
			if p := objc.GetProtocol(name); p != nil {
				protocols = append(protocols, p)
			}
		}
		scOutputClass, sckErr = objc.RegisterClass("LongarmStreamOutput", objc.GetClass("NSObject"), protocols, nil, []objc.MethodDef{
			{Cmd: sel("stream:didOutputSampleBuffer:ofType:"), Fn: scDidOutput},
			{Cmd: sel("stream:didStopWithError:"), Fn: scDidStop},
		})
	})
	return sckErr
}

func available() string {
	if err := macLoad(); err != nil {
		return "this macOS lacks a framework the remote desktop needs: " + err.Error()
	}
	if d := cgSessionCopyCurrentDict(); d != 0 {
		cfRelease(d)
	} else {
		return "longarmd runs outside a user's login session (a LaunchDaemon), which cannot see the screen: the remote desktop needs longarmd running as the signed-in user (LaunchAgent)"
	}
	if err := sckLoad(); err != nil {
		return err.Error()
	}
	if cgPreflightScreenCapture == nil || !cgPreflightScreenCapture() {
		return "macOS has not allowed Longarm to record the screen: System Settings > Privacy & Security > Screen Recording (longarmd)"
	}
	ds, err := Displays()
	if err != nil {
		return "no display: " + err.Error()
	}
	if len(ds) == 0 {
		return "no display attached"
	}
	return ""
}

// macSource captures a display with an SCStream: frames come on a
// dispatch queue (only when the screen changes) and are copied into back;
// Next swaps it in.
type macSource struct {
	d      macDisplay
	stream objc.ID
	output objc.ID
	queue  uintptr

	mu    sync.Mutex
	back  []byte
	fresh bool
	lost  bool
	ready chan struct{}

	frame  []byte
	cursor Cursor
}

func openSource(display int) (Source, error) {
	if why := available(); why != "" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, why)
	}
	ds, err := macDisplays()
	if err != nil {
		return nil, err
	}
	if display < 0 || display >= len(ds) {
		return nil, fmt.Errorf("no display %d (there are %d)", display, len(ds))
	}
	s := &macSource{d: ds[display], ready: make(chan struct{}, 1)}
	s.back = make([]byte, s.d.W*s.d.H*4)
	s.frame = make([]byte, s.d.W*s.d.H*4)
	var openErr error
	withPool(func() { openErr = s.start() })
	if openErr != nil {
		s.Close()
		return nil, openErr
	}
	return s, nil
}

// waitBlock waits for a completion handler that reports an NSError.
func waitBlock(what string, call func(objc.Block)) error {
	done := make(chan error, 1)
	b := objc.NewBlock(func(_ objc.Block, e objc.ID) {
		if e != 0 {
			done <- fmt.Errorf("%s: %s", what, nsError(e))
			return
		}
		done <- nil
	})
	defer b.Release()
	call(b)
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		return fmt.Errorf("%s: no answer in 10 s", what)
	}
}

func (s *macSource) start() error {
	// The displays and windows ScreenCaptureKit may capture.
	type result struct {
		content objc.ID
		err     error
	}
	got := make(chan result, 1)
	b := objc.NewBlock(func(_ objc.Block, content, e objc.ID) {
		if e != 0 || content == 0 {
			got <- result{err: fmt.Errorf("ScreenCaptureKit: %s", nsError(e))}
			return
		}
		got <- result{content: send(content, "retain")}
	})
	defer b.Release()
	send(class("SCShareableContent"), "getShareableContentWithCompletionHandler:", b)
	var r result
	select {
	case r = <-got:
	case <-time.After(10 * time.Second):
		return errors.New("ScreenCaptureKit: no shareable content in 10 s")
	}
	if r.err != nil {
		return r.err
	}
	defer send(r.content, "release")

	var scDisplay objc.ID
	displays := send(r.content, "displays")
	for i := 0; i < sendT[int](displays, "count"); i++ {
		d := send(displays, "objectAtIndex:", i)
		if sendT[uint32](d, "displayID") == s.d.id {
			scDisplay = d
			break
		}
	}
	if scDisplay == 0 {
		return fmt.Errorf("ScreenCaptureKit does not offer display %d", s.d.id)
	}
	filter := send(send(class("SCContentFilter"), "alloc"), "initWithDisplay:excludingWindows:", scDisplay, send(class("NSArray"), "array"))
	defer send(filter, "release")
	cfg := send(class("SCStreamConfiguration"), "new")
	defer send(cfg, "release")
	send(cfg, "setWidth:", uintptr(s.d.W))
	send(cfg, "setHeight:", uintptr(s.d.H))
	send(cfg, "setPixelFormat:", fourCC("BGRA"))
	send(cfg, "setMinimumFrameInterval:", cmTime{Value: 1, Timescale: 60, Flags: cmTimeValid})
	send(cfg, "setShowsCursor:", false) // the viewer draws the pointer
	send(cfg, "setQueueDepth:", 3)
	send(cfg, "setColorSpaceName:", objc.ID(kCGColorSpaceSRGB)) // the picture's colours as sRGB, like the other OSes

	s.output = send(objc.ID(scOutputClass), "new")
	scSources.Store(s.output, s)
	s.stream = send(send(class("SCStream"), "alloc"), "initWithFilter:configuration:delegate:", filter, cfg, s.output)
	if s.stream == 0 {
		return errors.New("SCStream: cannot create the stream")
	}
	s.queue = dispatchQueueCreate("longarm.screen\x00", 0)
	var e objc.ID
	const scStreamOutputTypeScreen = 0
	if !sendT[bool](s.stream, "addStreamOutput:type:sampleHandlerQueue:error:", s.output, scStreamOutputTypeScreen, s.queue, unsafe.Pointer(&e)) {
		return fmt.Errorf("SCStream addStreamOutput: %s", nsError(e))
	}
	return waitBlock("SCStream start", func(b objc.Block) { send(s.stream, "startCaptureWithCompletionHandler:", b) })
}

// scDidOutput is SCStreamOutput's stream:didOutputSampleBuffer:ofType:
// (on the capture queue).
func scDidOutput(self objc.ID, _ objc.SEL, _ objc.ID, sb unsafe.Pointer, typ int) {
	v, ok := scSources.Load(self)
	if !ok || typ != 0 || sb == nil {
		return
	}
	s := v.(*macSource)
	buf := uintptr(sb)
	// Only complete frames carry a new picture (idle: nothing changed).
	if att := cmSampleBufferGetSampleAttachments(buf, false); att != 0 && cfArrayGetCount(att) > 0 {
		if st := cfDictionaryGetValue(cfArrayGetValueAt(att, 0), scFrameStatus); st != 0 {
			const kCFNumberSInt64Type, scFrameStatusComplete = 4, 0
			var status int64
			cfNumberGetValue(st, kCFNumberSInt64Type, unsafe.Pointer(&status))
			if status != scFrameStatusComplete {
				return
			}
		}
	}
	pb := cmSampleBufferGetImageBuffer(buf)
	if pb == 0 {
		return
	}
	const readOnly = 1
	if cvPixelBufferLockBaseAddress(pb, readOnly) != 0 {
		return
	}
	s.mu.Lock()
	copyPixels(s.back, s.d.W, s.d.H, pb)
	s.fresh = true
	s.mu.Unlock()
	cvPixelBufferUnlockBaseAddress(pb, readOnly)
	signalReady(s.ready)
}

// scDidStop is SCStreamDelegate's stream:didStopWithError: (the stream
// ended: the display went away, permission was taken back).
func scDidStop(self objc.ID, _ objc.SEL, _ objc.ID, _ objc.ID) {
	if v, ok := scSources.Load(self); ok {
		s := v.(*macSource)
		s.mu.Lock()
		s.lost = true
		s.mu.Unlock()
		signalReady(s.ready)
	}
}

func signalReady(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (s *macSource) Display() Display { return s.d.Display }
func (s *macSource) Size() (int, int) { return s.d.W, s.d.H }
func (s *macSource) Frame() []byte    { return s.frame }
func (s *macSource) Cursor() Cursor   { return s.cursor }

func (s *macSource) Next(timeout time.Duration) (bool, error) {
	t := time.NewTimer(timeout)
	select {
	case <-s.ready:
	case <-t.C:
	}
	t.Stop()
	s.mu.Lock()
	lost, changed := s.lost, s.fresh
	if changed {
		s.back, s.frame = s.frame, s.back
		s.fresh = false
	}
	s.mu.Unlock()
	if lost {
		return false, ErrAccessLost
	}
	// A new mode (resolution, scaling): the stream keeps its size, so
	// start again with the new one.
	if m := cgDisplayCopyDisplayMode(s.d.id); m != 0 {
		w, h := int(cgDisplayModeGetPixelWidth(m)), int(cgDisplayModeGetPixelHeight(m))
		cgDisplayModeRelease(m)
		if w != s.d.W || h != s.d.H {
			return false, ErrAccessLost
		}
	}
	s.cursor = s.pointer()
	return changed, nil
}

// pointer is the mouse position on this display (the shape is the
// viewer's own arrow: todo/remote-desktop.md).
func (s *macSource) pointer() Cursor {
	ev := cgEventCreate(0)
	if ev == 0 {
		return Cursor{}
	}
	p := cgEventGetLocation(ev)
	cfRelease(ev)
	b := s.d.pts
	in := p.X >= b.Origin.X && p.Y >= b.Origin.Y && p.X < b.Origin.X+b.Size.W && p.Y < b.Origin.Y+b.Size.H
	return Cursor{X: int((p.X - b.Origin.X) * s.d.scale), Y: int((p.Y - b.Origin.Y) * s.d.scale), Visible: in}
}

func (s *macSource) Close() {
	withPool(func() {
		if s.stream != 0 {
			// An error here is a stream that never started: nothing to stop.
			waitBlock("SCStream stop", func(b objc.Block) { send(s.stream, "stopCaptureWithCompletionHandler:", b) })
			send(s.stream, "release")
			s.stream = 0
		}
		if s.output != 0 {
			scSources.Delete(s.output)
			send(s.output, "release")
			s.output = 0
		}
		if s.queue != 0 {
			dispatchRelease(s.queue)
			s.queue = 0
		}
	})
}
