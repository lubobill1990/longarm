//go:build darwin

package screen

import (
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

// The viewer is an NSWindow whose content view is layer-backed: each
// picture becomes a CGImage set as the layer's contents, which Core
// Animation scales and letterboxes (resizeAspect) on the GPU. The view is
// a class registered at run time (LongarmDesktopView) for the mouse and
// the window's delegate calls. Keys are taken in the event loop before
// AppKit dispatches them: that is the only way to see key-ups while
// Command is held, and it keeps AppKit's key equivalents and beeps out.

// The viewer window exists on macOS.
const canView = true

var (
	appKitOnce sync.Once
	appKitErr  error
	viewClass  objc.Class
	caGravity  uintptr // kCAGravityResizeAspect
	theViewer  *macViewer
)

func appKitLoad() error {
	appKitOnce.Do(func() {
		if appKitErr = macLoad(); appKitErr != nil {
			return
		}
		ak, qc := framework("AppKit"), framework("QuartzCore")
		caGravity = qc.obj("kCAGravityResizeAspect")
		for _, l := range []*lib{ak, qc} {
			if l.err != nil {
				appKitErr = l.err
				return
			}
		}
		mouse := func(kind int) func(objc.ID, objc.SEL, objc.ID) {
			return func(_ objc.ID, _ objc.SEL, ev objc.ID) {
				if v := theViewer; v != nil {
					v.mouse(kind, ev)
				}
			}
		}
		yes := func(objc.ID, objc.SEL) bool { return true }
		viewClass, appKitErr = objc.RegisterClass("LongarmDesktopView", objc.GetClass("NSView"), nil, nil, []objc.MethodDef{
			{Cmd: sel("acceptsFirstResponder"), Fn: yes},
			{Cmd: sel("acceptsFirstMouse:"), Fn: func(objc.ID, objc.SEL, objc.ID) bool { return true }},
			{Cmd: sel("mouseDown:"), Fn: mouse(mLeftDown)},
			{Cmd: sel("mouseUp:"), Fn: mouse(mLeftUp)},
			{Cmd: sel("rightMouseDown:"), Fn: mouse(mRightDown)},
			{Cmd: sel("rightMouseUp:"), Fn: mouse(mRightUp)},
			{Cmd: sel("otherMouseDown:"), Fn: mouse(mOtherDown)},
			{Cmd: sel("otherMouseUp:"), Fn: mouse(mOtherUp)},
			{Cmd: sel("mouseMoved:"), Fn: mouse(mMove)},
			{Cmd: sel("mouseDragged:"), Fn: mouse(mMove)},
			{Cmd: sel("rightMouseDragged:"), Fn: mouse(mMove)},
			{Cmd: sel("otherMouseDragged:"), Fn: mouse(mMove)},
			{Cmd: sel("scrollWheel:"), Fn: mouse(mWheel)},
			// performSelectorOnMainThread: target (packets arrived).
			{Cmd: sel("longarmDrain:"), Fn: func(objc.ID, objc.SEL, objc.ID) {
				if v := theViewer; v != nil {
					v.drain()
				}
			}},
			// NSWindowDelegate.
			{Cmd: sel("windowWillClose:"), Fn: func(objc.ID, objc.SEL, objc.ID) {
				if v := theViewer; v != nil {
					v.closed = true
				}
			}},
			{Cmd: sel("windowDidResignKey:"), Fn: func(objc.ID, objc.SEL, objc.ID) {
				if v := theViewer; v != nil {
					v.releaseKeys()
				}
			}},
		})
	})
	return appKitErr
}

const (
	mMove = iota
	mLeftDown
	mLeftUp
	mRightDown
	mRightUp
	mOtherDown
	mOtherUp
	mWheel
)

// NSEvent types and modifier flags (NSEvent.h).
const (
	nsKeyDown      = 10
	nsKeyUp        = 11
	nsFlagsChanged = 12

	nsFlagControl = 1 << 18
	nsFlagOption  = 1 << 19
)

type macViewer struct {
	o      ViewerOptions
	app    objc.ID
	win    objc.ID
	view   objc.ID
	layer  objc.ID
	cs     uintptr // sRGB
	closed bool

	mu      sync.Mutex
	queue   []VideoPacket
	ended   bool
	pending atomic.Bool // a drain is queued on the main thread

	cfg      VideoPacket
	dec      VideoDecoder
	img      []byte
	cursor   objc.ID // NSCursor of the worker's pointer shape
	held     map[uint16]bool
	wheel    [2]float64 // scrolled, not sent yet (1/120ths of a notch)
	lastTick time.Time
}

func runViewer(o ViewerOptions) error {
	if err := appKitLoad(); err != nil {
		return err
	}
	v := &macViewer{o: o, held: map[uint16]bool{}}
	theViewer = v
	defer func() { theViewer = nil }()
	v.cs = cgColorSpaceCreateWithName(kCGColorSpaceSRGB)
	defer cgColorSpaceRelease(v.cs)
	withPool(v.open)
	defer func() {
		if v.dec != nil {
			v.dec.Close()
		}
		if v.cursor != 0 {
			send(v.cursor, "release")
		}
	}()

	go func() {
		for p := range o.Packets {
			v.mu.Lock()
			v.queue = append(v.queue, p)
			v.mu.Unlock()
			v.wake()
		}
		v.mu.Lock()
		v.ended = true
		v.mu.Unlock()
		v.wake()
	}()

	var mode objc.ID
	withPool(func() { mode = send(nsString("kCFRunLoopDefaultMode"), "retain") }) // NSDefaultRunLoopMode
	defer send(mode, "release")
	for !v.closed {
		withPool(func() {
			// Wait for an event, or a quarter second (the status, the end).
			until := send(class("NSDate"), "dateWithTimeIntervalSinceNow:", 0.25)
			ev := send(v.app, "nextEventMatchingMask:untilDate:inMode:dequeue:", uint64(math.MaxUint64), until, mode, true)
			if ev != 0 && !v.key(ev) {
				send(v.app, "sendEvent:", ev)
			}
			v.drain()
			v.mu.Lock()
			ended := v.ended && len(v.queue) == 0
			v.mu.Unlock()
			if ended && !v.closed {
				send(v.win, "close")
			}
			if v.o.Status != nil && time.Since(v.lastTick) >= time.Second {
				v.lastTick = time.Now()
				if s := v.o.Status(); s != "" {
					send(v.win, "setTitle:", nsString(v.o.Title+" - "+s))
				}
			}
		})
	}
	v.releaseKeys()
	send(v.win, "release")
	return nil
}

var appLaunched sync.Once

func (v *macViewer) open() {
	v.app = send(class("NSApplication"), "sharedApplication")
	appLaunched.Do(func() {
		const regular = 0 // a Dock icon and a menu bar, so the window can be in front
		send(v.app, "setActivationPolicy:", regular)
		send(v.app, "finishLaunching")
	})
	const (
		titled, closable, miniaturizable, resizable = 1, 2, 4, 8
		backingBuffered                             = 2
		fullScreenPrimary                           = 1 << 7
	)
	r := cgRect{Size: cgSize{W: 1280, H: 800}}
	v.win = send(send(class("NSWindow"), "alloc"), "initWithContentRect:styleMask:backing:defer:",
		r, uint(titled|closable|miniaturizable|resizable), uint(backingBuffered), false)
	send(v.win, "setReleasedWhenClosed:", false)
	send(v.win, "setCollectionBehavior:", uint(fullScreenPrimary)) // the green button
	send(v.win, "setAcceptsMouseMovedEvents:", true)
	send(v.win, "setBackgroundColor:", send(class("NSColor"), "blackColor")) // the bars
	send(v.win, "setTitle:", nsString(v.o.Title))
	v.view = send(send(objc.ID(viewClass), "alloc"), "initWithFrame:", r)
	send(v.view, "setWantsLayer:", true)
	v.layer = send(v.view, "layer")
	send(v.layer, "setContentsGravity:", objc.ID(caGravity))
	// The view stays retained (not released here): the packet goroutine
	// may still perform a drain on it after the window has gone.
	send(v.win, "setContentView:", v.view)
	send(v.win, "setDelegate:", v.view)
	send(v.win, "center")
	send(v.win, "makeKeyAndOrderFront:", objc.ID(0))
	send(v.win, "makeFirstResponder:", v.view)
	send(v.app, "activateIgnoringOtherApps:", true)
}

// wake has the main thread drain the queue now (from the packet goroutine).
func (v *macViewer) wake() {
	if v.pending.Swap(true) {
		return
	}
	withPool(func() {
		send(v.view, "performSelectorOnMainThread:withObject:waitUntilDone:", sel("longarmDrain:"), objc.ID(0), false)
	})
}

// drain handles the packets that came (on the main thread).
func (v *macViewer) drain() {
	v.pending.Store(false)
	v.mu.Lock()
	q := v.queue
	v.queue = nil
	v.mu.Unlock()
	for _, p := range q {
		v.handle(p)
	}
}

func (v *macViewer) handle(p VideoPacket) {
	switch {
	case p.Cursor != nil:
		if p.Cursor.BGRA != nil {
			v.setCursor(p.Cursor)
		}
		return
	case p.SecureHiddenOK:
		return
	case p.Config:
		if v.dec != nil {
			v.dec.Close()
			v.dec = nil
		}
		v.cfg = p
		v.img = make([]byte, p.Width*p.Height*4)
		if p.Codec == "h264" {
			var err error
			if v.dec, err = NewH264Decoder(p.Width, p.Height); err != nil {
				slog.Error("desktop viewer: no H.264 decoder", "err", err)
				v.dec = nil
			}
		}
		return
	}
	shown := false
	switch {
	case v.cfg.Codec == "h264" && v.dec != nil:
		img, _, _, err := v.dec.Decode(p.Data)
		if err != nil {
			slog.Debug("desktop viewer: decode", "err", err)
			if v.o.NeedKeyframe != nil {
				v.o.NeedKeyframe()
			}
		}
		if img != nil {
			copy(v.img, img)
			shown = true
		}
	case v.cfg.Codec == "jpeg" && v.img != nil:
		DrawTiles(v.img, v.cfg.Width, v.cfg.Height, p.Tiles)
		shown = true
	}
	if !shown {
		return
	}
	v.present()
	if v.o.Shown != nil {
		v.o.Shown(p.Seq)
	}
}

// present makes the picture the layer's contents.
func (v *macViewer) present() {
	w, h := v.cfg.Width, v.cfg.Height
	if w <= 0 || h <= 0 || len(v.img) < w*h*4 {
		return
	}
	// BGRA in memory is 32-bit little-endian xRGB.
	const byteOrder32Little, alphaNoneSkipFirst = 2 << 12, 6
	data := cfDataCreate(0, unsafe.Pointer(&v.img[0]), w*h*4) // a copy: Core Animation reads it later
	prov := cgDataProviderCreateWithData(data)
	cfRelease(data)
	img := cgImageCreate(uintptr(w), uintptr(h), 8, 32, uintptr(w*4), v.cs, byteOrder32Little|alphaNoneSkipFirst, prov, nil, false, 0)
	cgDataProviderRelease(prov)
	if img == 0 {
		return
	}
	ca := class("CATransaction")
	send(ca, "begin")
	send(ca, "setDisableActions:", true) // no cross-fade between frames
	send(v.layer, "setContents:", objc.ID(img))
	send(ca, "commit")
	cgImageRelease(img)
}

// setCursor makes the worker's pointer shape this window's cursor.
func (v *macViewer) setCursor(c *Cursor) {
	if c.W <= 0 || c.H <= 0 || len(c.BGRA) < c.W*c.H*4 {
		return
	}
	const byteOrder32Little, alphaFirst = 2 << 12, 2 // straight alpha
	data := cfDataCreate(0, unsafe.Pointer(&c.BGRA[0]), c.W*c.H*4)
	prov := cgDataProviderCreateWithData(data)
	cfRelease(data)
	img := cgImageCreate(uintptr(c.W), uintptr(c.H), 8, 32, uintptr(c.W*4), v.cs, byteOrder32Little|alphaFirst, prov, nil, false, 0)
	cgDataProviderRelease(prov)
	if img == 0 {
		return
	}
	defer cgImageRelease(img)
	// The worker's pixels are this screen's pixels: size it in points.
	scale := max(sendT[float64](v.win, "backingScaleFactor"), 1)
	size := cgSize{W: float64(c.W) / scale, H: float64(c.H) / scale}
	nsImg := send(send(class("NSImage"), "alloc"), "initWithCGImage:size:", objc.ID(img), size)
	cur := send(send(class("NSCursor"), "alloc"), "initWithImage:hotSpot:", nsImg, cgPoint{X: float64(c.HotX) / scale, Y: float64(c.HotY) / scale})
	send(nsImg, "release")
	if v.cursor != 0 {
		send(v.cursor, "release")
	}
	v.cursor = cur
}

// mouse sends a mouse event of the view (on the main thread).
func (v *macViewer) mouse(kind int, ev objc.ID) {
	if v.cursor != 0 {
		send(v.cursor, "set")
	}
	if v.o.Input == nil {
		return
	}
	if kind == mWheel {
		dx, dy := sendT[float64](ev, "scrollingDeltaX"), sendT[float64](ev, "scrollingDeltaY")
		per := 120.0 // a line (a notch)
		if sendT[bool](ev, "hasPreciseScrollingDeltas") {
			per = 3 // trackpad points: ~40 to a notch
		}
		// AppKit: positive x scrolls left; the wire's positive x is right.
		v.wheel[0] -= dx * per
		v.wheel[1] += dy * per
		wx, wy := int(v.wheel[0]), int(v.wheel[1])
		if wx == 0 && wy == 0 {
			return
		}
		v.wheel[0] -= float64(wx)
		v.wheel[1] -= float64(wy)
		v.o.Input(InputEvent{Mouse: &MouseEvent{WheelX: wx, WheelY: wy}})
		return
	}
	p := sendT[cgPoint](v.view, "convertPoint:fromView:", sendT[cgPoint](ev, "locationInWindow"), objc.ID(0))
	b := sendT[cgRect](v.view, "bounds")
	x, y, ok := macToRemote(p.X, b.Size.H-p.Y, b.Size.W, b.Size.H, v.cfg.Width, v.cfg.Height) // AppKit's y goes up
	if !ok {
		return
	}
	m := &MouseEvent{X: x, Y: y}
	switch kind {
	case mLeftDown, mLeftUp:
		m.Button, m.Down = ButtonLeft, kind == mLeftDown
	case mRightDown, mRightUp:
		m.Button, m.Down = ButtonRight, kind == mRightDown
	case mOtherDown, mOtherUp:
		if sendT[int](ev, "buttonNumber") != 2 {
			return
		}
		m.Button, m.Down = ButtonMiddle, kind == mOtherDown
	}
	v.o.Input(InputEvent{Mouse: m})
}

// key takes the window's key events; false: not one (AppKit handles it).
func (v *macViewer) key(ev objc.ID) bool {
	t := sendT[uint](ev, "type")
	if t != nsKeyDown && t != nsKeyUp && t != nsFlagsChanged {
		return false
	}
	if send(ev, "window") != v.win {
		return false
	}
	code := sendT[uint16](ev, "keyCode")
	flags := sendT[uint](ev, "modifierFlags")
	// Ctrl+Option+Enter: full screen, here (Ctrl+Alt+Enter on Windows).
	if t == nsKeyDown && (code == 0x24 || code == 0x4C) && flags&nsFlagControl != 0 && flags&nsFlagOption != 0 {
		send(v.win, "toggleFullScreen:", objc.ID(0))
		return true
	}
	// Ctrl+Option+End: Ctrl+Alt+Del on a Windows worker.
	if t == nsKeyDown && code == 0x77 && flags&nsFlagControl != 0 && flags&nsFlagOption != 0 {
		if v.o.Input != nil {
			v.o.Input(InputEvent{SAS: true})
		}
		return true
	}
	if v.o.Input == nil {
		return true
	}
	scan, ok := macScan(code)
	if !ok {
		return true
	}
	switch t {
	case nsKeyDown:
		v.sendKey(scan, true)
	case nsKeyUp:
		v.sendKey(scan, false)
	case nsFlagsChanged:
		if code == 0x39 { // Caps Lock: each change is a press
			v.sendKey(scan, true)
			v.sendKey(scan, false)
			return true
		}
		if m := macModifier(code); m != 0 {
			v.sendKey(scan, uint64(flags)&m&0xFFFF != 0) // the left/right key's own bit
		}
	}
	return true
}

func (v *macViewer) sendKey(scan uint16, down bool) {
	if down {
		v.held[scan] = true
	} else {
		delete(v.held, scan)
	}
	v.o.Input(InputEvent{Key: &KeyEvent{Scan: scan, Down: down}})
}

// releaseKeys lets go of every key held on the worker (the window lost
// the keyboard, or closes).
func (v *macViewer) releaseKeys() {
	if v.o.Input == nil {
		return
	}
	for scan := range v.held {
		v.sendKey(scan, false)
	}
}
