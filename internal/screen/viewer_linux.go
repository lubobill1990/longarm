//go:build linux

package screen

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/render"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"
	"golang.org/x/sys/unix"
)

// canView: the viewer is an X11 window; it needs an X display (checked
// when the client asks, see CanView's callers: they get an error from
// RunViewer otherwise).
var canView = os.Getenv("DISPLAY") != "" && !wayland()

// The Linux viewer decodes in software (OpenH264, or JPEG tiles), scales
// into the window (letterboxed, nearest neighbour, in parallel strips) and
// puts the picture with MIT-SHM (PutImage in pieces without it). The
// pointer is the local one, with the worker's cursor shape.
type x11Viewer struct {
	o    ViewerOptions
	c    *xgb.Conn
	scr  *xproto.ScreenInfo
	win  xproto.Window
	gc   xproto.Gcontext
	maxR int // PutImage bytes per request

	pw, ph int    // the picture
	codec  string // its codec
	img    []byte // BGRA pw×ph
	dec    VideoDecoder
	have   bool // img holds a picture

	ww, wh int     // the window
	seg    shm.Seg // the window-sized buffer, shared with the server
	buf    []byte
	xmap   []int // window column → picture column (scaled area only)
	box    Rect  // the picture's place in the window

	atoms      map[string]xproto.Atom
	fullscreen bool
	down       map[uint16]bool // keys pressed on the worker
	argb32     render.Pictformat
	cursor     xproto.Cursor // the worker's shape (0: the default)
	blank      xproto.Cursor
	secure     bool
	sized      bool
}

func runViewer(o ViewerOptions) error {
	c, err := xConnect()
	if err != nil {
		return err
	}
	defer c.Close()
	scr := xproto.Setup(c).DefaultScreen(c)
	if err := xFormat(c, scr); err != nil {
		return err
	}
	v := &x11Viewer{o: o, c: c, scr: scr, down: map[uint16]bool{}, atoms: map[string]xproto.Atom{}, ww: 1024, wh: 640}
	v.maxR = int(xproto.Setup(c).MaximumRequestLength)*4 - 64
	if err := v.open(); err != nil {
		return err
	}
	defer v.close()

	events := make(chan xgb.Event, 64)
	go func() {
		defer close(events)
		for {
			ev, err := c.WaitForEvent()
			if ev == nil && err == nil {
				return
			}
			if ev != nil {
				events <- ev
			}
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var shown uint64
	for {
		select {
		case p, ok := <-o.Packets:
			if !ok {
				return nil
			}
			seq, err := v.packet(p)
			if err != nil {
				return err
			}
			shown = max(shown, seq)
			if len(o.Packets) > 0 {
				continue // draw the newest only
			}
			if shown != 0 {
				v.present()
				if o.Shown != nil {
					o.Shown(shown)
				}
				shown = 0
			}
		case ev, ok := <-events:
			if !ok {
				return nil // the X connection went
			}
			if done := v.event(ev); done {
				return nil
			}
		case <-tick.C:
			v.title()
		}
	}
}

func (v *x11Viewer) atom(name string) xproto.Atom {
	a, ok := v.atoms[name]
	if !ok {
		a = xAtom(v.c, name)
		v.atoms[name] = a
	}
	return a
}

func (v *x11Viewer) open() error {
	id, err := v.c.NewId()
	if err != nil {
		return err
	}
	v.win = xproto.Window(id)
	mask := uint32(xproto.EventMaskExposure | xproto.EventMaskStructureNotify | xproto.EventMaskKeyPress |
		xproto.EventMaskKeyRelease | xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease |
		xproto.EventMaskPointerMotion | xproto.EventMaskFocusChange)
	if err := xproto.CreateWindowChecked(v.c, v.scr.RootDepth, v.win, v.scr.Root, 0, 0, uint16(v.ww), uint16(v.wh), 0,
		xproto.WindowClassInputOutput, v.scr.RootVisual, xproto.CwBackPixel|xproto.CwEventMask,
		[]uint32{v.scr.BlackPixel, mask}).Check(); err != nil {
		return fmt.Errorf("X window: %w", err)
	}
	del := v.atom("WM_DELETE_WINDOW")
	xproto.ChangeProperty(v.c, xproto.PropModeReplace, v.win, v.atom("WM_PROTOCOLS"), xproto.AtomAtom, 32, 1, u32bytes(uint32(del)))
	cls := "longarm\x00Longarm\x00"
	xproto.ChangeProperty(v.c, xproto.PropModeReplace, v.win, xproto.AtomWmClass, xproto.AtomString, 8, uint32(len(cls)), []byte(cls))
	v.setTitle(v.o.Title)
	gc, err := v.c.NewId()
	if err != nil {
		return err
	}
	v.gc = xproto.Gcontext(gc)
	xproto.CreateGC(v.c, v.gc, xproto.Drawable(v.win), 0, nil)
	v.cursors()
	xproto.MapWindow(v.c, v.win)
	v.resize(v.ww, v.wh)
	return nil
}

func (v *x11Viewer) close() {
	if v.dec != nil {
		v.dec.Close()
	}
	v.freeBuf()
	xproto.DestroyWindow(v.c, v.win)
	v.c.Sync()
}

func u32bytes(vals ...uint32) []byte {
	b := make([]byte, 4*len(vals))
	for i, x := range vals {
		xgb.Put32(b[4*i:], x)
	}
	return b
}

func (v *x11Viewer) setTitle(t string) {
	xproto.ChangeProperty(v.c, xproto.PropModeReplace, v.win, v.atom("_NET_WM_NAME"), v.atom("UTF8_STRING"), 8, uint32(len(t)), []byte(t))
	xproto.ChangeProperty(v.c, xproto.PropModeReplace, v.win, xproto.AtomWmName, xproto.AtomString, 8, uint32(len(t)), []byte(t))
}

func (v *x11Viewer) title() {
	t := v.o.Title
	if v.o.Status != nil {
		if s := v.o.Status(); s != "" {
			t += " - " + s
		}
	}
	if v.secure {
		t += " - secure screen (not shown)"
	}
	v.setTitle(t)
}

// packet takes one packet from the worker; it returns the frame's Seq once
// the picture changed (0 otherwise).
func (v *x11Viewer) packet(p VideoPacket) (uint64, error) {
	switch {
	case p.Config:
		v.pw, v.ph, v.codec = p.Width, p.Height, p.Codec
		v.img = make([]byte, p.Width*p.Height*4)
		v.have = false
		if v.dec != nil {
			v.dec.Close()
			v.dec = nil
		}
		if p.Codec == "h264" {
			var err error
			if v.dec, err = NewH264Decoder(p.Width, p.Height); err != nil {
				return 0, fmt.Errorf("H.264 decoder: %w", err)
			}
		}
		if !v.sized {
			v.sized = true
			v.fitWindow()
		}
		v.resize(v.ww, v.wh)
		return 0, nil
	case p.Cursor != nil:
		v.setCursor(p.Cursor)
		return 0, nil
	case p.SecureHiddenOK:
		v.secure = p.SecureHidden
		v.title()
		return 0, nil
	}
	if v.img == nil {
		return 0, nil
	}
	switch v.codec {
	case "h264":
		out, w, h, err := v.dec.Decode(p.Data)
		if err != nil {
			if v.o.NeedKeyframe != nil {
				v.o.NeedKeyframe()
			}
			return p.Seq, nil // ack it all the same: the worker must not stall
		}
		if out == nil {
			return p.Seq, nil
		}
		for y := 0; y < min(h, v.ph); y++ {
			copy(v.img[y*v.pw*4:(y+1)*v.pw*4], out[y*w*4:])
		}
	case "jpeg":
		DrawTiles(v.img, v.pw, v.ph, p.Tiles)
	}
	v.have = true
	return max(p.Seq, 1), nil
}

// fitWindow sizes the window for the picture, within 90% of the screen.
func (v *x11Viewer) fitWindow() {
	sw, sh := int(v.scr.WidthInPixels)*9/10, int(v.scr.HeightInPixels)*9/10
	w, h := v.pw, v.ph
	if w > sw || h > sh {
		if w*sh > h*sw {
			w, h = sw, h*sw/w
		} else {
			w, h = w*sh/h, sh
		}
	}
	xproto.ConfigureWindow(v.c, v.win, xproto.ConfigWindowWidth|xproto.ConfigWindowHeight, []uint32{uint32(max(w, 64)), uint32(max(h, 64))})
}

func (v *x11Viewer) freeBuf() {
	if v.seg != 0 {
		shm.Detach(v.c, v.seg)
		unix.SysvShmDetach(v.buf)
		v.seg = 0
	}
	v.buf = nil
}

// resize makes the window's buffer and the letterbox for a new size.
func (v *x11Viewer) resize(w, h int) {
	w, h = max(w, 1), max(h, 1)
	if w != v.ww || h != v.wh || v.buf == nil {
		v.freeBuf()
		v.ww, v.wh = w, h
		if v.seg, v.buf = xShm(v.c, w*h*4); v.seg == 0 {
			v.buf = make([]byte, w*h*4)
		}
	}
	v.box = Rect{W: w, H: h}
	if v.pw > 0 && v.ph > 0 {
		if w*v.ph > h*v.pw {
			v.box.W = max(v.pw*h/v.ph, 1)
		} else {
			v.box.H = max(v.ph*w/v.pw, 1)
		}
		v.box.X, v.box.Y = (w-v.box.W)/2, (h-v.box.H)/2
	}
	v.xmap = make([]int, v.box.W)
	for x := range v.xmap {
		v.xmap[x] = min(x*v.pw/v.box.W, max(v.pw-1, 0))
	}
}

// present scales the picture into the window.
func (v *x11Viewer) present() {
	if v.buf == nil {
		return
	}
	w, h, b := v.ww, v.wh, v.box
	strips := min(runtime.GOMAXPROCS(0), h)
	rows := (h + strips - 1) / strips
	var wg sync.WaitGroup
	for s := 0; s < strips; s++ {
		y0, y1 := s*rows, min((s+1)*rows, h)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for y := y0; y < y1; y++ {
				row := v.buf[y*w*4 : (y+1)*w*4]
				if !v.have || y < b.Y || y >= b.Y+b.H {
					clear(row)
					continue
				}
				clear(row[:b.X*4])
				clear(row[(b.X+b.W)*4:])
				src := v.img[min((y-b.Y)*v.ph/b.H, v.ph-1)*v.pw*4:]
				dst := row[b.X*4:]
				if b.W == v.pw {
					copy(dst[:b.W*4], src)
					continue
				}
				for x, sx := range v.xmap {
					copy(dst[x*4:x*4+4], src[sx*4:sx*4+4])
				}
			}
		}()
	}
	wg.Wait()
	if v.seg != 0 {
		// Checked: the server has read the buffer before we touch it again.
		shm.PutImageChecked(v.c, xproto.Drawable(v.win), v.gc, uint16(w), uint16(h), 0, 0, uint16(w), uint16(h), 0, 0,
			v.scr.RootDepth, xproto.ImageFormatZPixmap, 0, v.seg, 0).Check()
		return
	}
	per := max(v.maxR/(w*4), 1)
	for y := 0; y < h; y += per {
		n := min(per, h-y)
		xproto.PutImage(v.c, xproto.ImageFormatZPixmap, xproto.Drawable(v.win), v.gc, uint16(w), uint16(n), 0, int16(y), 0,
			v.scr.RootDepth, v.buf[y*w*4:(y+n)*w*4])
	}
}

// toPicture maps window coordinates to the worker's display.
func (v *x11Viewer) toPicture(x, y int) (int, int) {
	b := v.box
	if b.W == 0 || b.H == 0 {
		return 0, 0
	}
	px := min(max((x-b.X)*v.pw/b.W, 0), v.pw-1)
	py := min(max((y-b.Y)*v.ph/b.H, 0), v.ph-1)
	return px, py
}

const (
	scanCtrl, scanAlt, scanEnter, scanEnd = 0x1D, 0x38, 0x1C, 0x4F
)

// event handles an X event; true: the window was closed.
func (v *x11Viewer) event(ev xgb.Event) bool {
	switch e := ev.(type) {
	case xproto.ExposeEvent:
		if e.Count == 0 {
			v.present()
		}
	case xproto.ConfigureNotifyEvent:
		if e.Window == v.win && (int(e.Width) != v.ww || int(e.Height) != v.wh) {
			v.resize(int(e.Width), int(e.Height))
			v.present()
		}
	case xproto.ClientMessageEvent:
		if e.Format == 32 && xproto.Atom(e.Data.Data32[0]) == v.atom("WM_DELETE_WINDOW") {
			return true
		}
	case xproto.DestroyNotifyEvent:
		return e.Window == v.win
	case xproto.FocusOutEvent:
		v.releaseKeys()
	case xproto.KeyPressEvent:
		v.key(e.Detail, true)
	case xproto.KeyReleaseEvent:
		v.key(e.Detail, false)
	case xproto.ButtonPressEvent:
		v.button(byte(e.Detail), true, int(e.EventX), int(e.EventY))
	case xproto.ButtonReleaseEvent:
		v.button(byte(e.Detail), false, int(e.EventX), int(e.EventY))
	case xproto.MotionNotifyEvent:
		if v.o.Input != nil && v.have {
			x, y := v.toPicture(int(e.EventX), int(e.EventY))
			v.o.Input(InputEvent{Mouse: &MouseEvent{X: x, Y: y}})
		}
	}
	return false
}

func (v *x11Viewer) button(b byte, down bool, wx, wy int) {
	if v.o.Input == nil || !v.have {
		return
	}
	x, y := v.toPicture(wx, wy)
	switch b {
	case 1, 2, 3:
		v.o.Input(InputEvent{Mouse: &MouseEvent{X: x, Y: y, Button: int(b), Down: down}})
	case 4, 5, 6, 7:
		if !down {
			return
		}
		m := &MouseEvent{X: x, Y: y}
		switch b {
		case 4:
			m.WheelY = 120
		case 5:
			m.WheelY = -120
		case 6:
			m.WheelX = -120
		case 7:
			m.WheelX = 120
		}
		v.o.Input(InputEvent{Mouse: m})
	}
}

// key: Ctrl+Alt+Enter toggles full screen and Ctrl+Alt+End sends
// Ctrl+Alt+Del, as in the Windows viewer; the rest goes to the worker.
func (v *x11Viewer) key(code xproto.Keycode, down bool) {
	if v.o.Input == nil || code < 8 {
		return
	}
	scan := set1FromEvdev(uint16(code) - 8)
	if scan == 0 {
		return
	}
	ctrl := v.down[scanCtrl] || v.down[0xE000|scanCtrl]
	alt := v.down[scanAlt] || v.down[0xE000|scanAlt]
	if down && ctrl && alt {
		switch scan {
		case scanEnter:
			v.toggleFullscreen()
			return
		case 0xE000 | scanEnd:
			v.o.Input(InputEvent{SAS: true})
			return
		}
	}
	if down {
		v.down[scan] = true
	} else if !v.down[scan] {
		return // its press was a shortcut of ours
	} else {
		delete(v.down, scan)
	}
	v.o.Input(InputEvent{Key: &KeyEvent{Scan: scan, Down: down}})
}

// releaseKeys lets go of every key held on the worker (focus went
// elsewhere: the releases come to another window).
func (v *x11Viewer) releaseKeys() {
	for s := range v.down {
		v.o.Input(InputEvent{Key: &KeyEvent{Scan: s}})
	}
	clear(v.down)
}

// toggleFullscreen asks the window manager (_NET_WM_STATE); while full
// screen the keyboard is grabbed, so its shortcuts reach the worker.
func (v *x11Viewer) toggleFullscreen() {
	v.fullscreen = !v.fullscreen
	ev := xproto.ClientMessageEvent{Format: 32, Window: v.win, Type: v.atom("_NET_WM_STATE"),
		Data: xproto.ClientMessageDataUnionData32New([]uint32{2, uint32(v.atom("_NET_WM_STATE_FULLSCREEN")), 0, 1, 0})}
	xproto.SendEvent(v.c, false, v.scr.Root, xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify, string(ev.Bytes()))
	if v.fullscreen {
		xproto.GrabKeyboard(v.c, true, v.win, xproto.TimeCurrentTime, xproto.GrabModeAsync, xproto.GrabModeAsync)
	} else {
		xproto.UngrabKeyboard(v.c, xproto.TimeCurrentTime)
	}
}

// cursors prepares the worker's cursor shapes (RENDER's ARGB cursors).
func (v *x11Viewer) cursors() {
	if render.Init(v.c) != nil {
		return
	}
	if _, err := render.QueryVersion(v.c, 0, 11).Reply(); err != nil {
		return
	}
	r, err := render.QueryPictFormats(v.c).Reply()
	if err != nil {
		return
	}
	for _, f := range r.Formats {
		d := f.Direct
		if f.Type == render.PictTypeDirect && f.Depth == 32 && d.AlphaShift == 24 && d.AlphaMask == 0xFF &&
			d.RedShift == 16 && d.RedMask == 0xFF && d.GreenShift == 8 && d.BlueShift == 0 {
			v.argb32 = f.Id
		}
	}
	if v.argb32 != 0 {
		v.blank = v.argbCursor(1, 1, 0, 0, make([]byte, 4))
	}
}

// argbCursor makes a cursor of a straight-alpha BGRA image (0 on failure).
func (v *x11Viewer) argbCursor(w, h, hx, hy int, bgra []byte) xproto.Cursor {
	if v.argb32 == 0 || w <= 0 || h <= 0 || len(bgra) < w*h*4 || w*h*4 > v.maxR {
		return 0
	}
	pre := make([]byte, w*h*4) // RENDER wants premultiplied alpha
	for i := 0; i < w*h*4; i += 4 {
		a := uint32(bgra[i+3])
		pre[i], pre[i+1], pre[i+2], pre[i+3] = byte(uint32(bgra[i])*a/255), byte(uint32(bgra[i+1])*a/255), byte(uint32(bgra[i+2])*a/255), byte(a)
	}
	ids := make([]uint32, 4)
	for i := range ids {
		id, err := v.c.NewId()
		if err != nil {
			return 0
		}
		ids[i] = id
	}
	pix, gc, pic, cur := xproto.Pixmap(ids[0]), xproto.Gcontext(ids[1]), render.Picture(ids[2]), xproto.Cursor(ids[3])
	xproto.CreatePixmap(v.c, 32, pix, xproto.Drawable(v.win), uint16(w), uint16(h))
	xproto.CreateGC(v.c, gc, xproto.Drawable(pix), 0, nil)
	xproto.PutImage(v.c, xproto.ImageFormatZPixmap, xproto.Drawable(pix), gc, uint16(w), uint16(h), 0, 0, 0, 32, pre)
	render.CreatePicture(v.c, pic, xproto.Drawable(pix), v.argb32, 0, nil)
	err := render.CreateCursorChecked(v.c, cur, pic, uint16(hx), uint16(hy)).Check()
	render.FreePicture(v.c, pic)
	xproto.FreeGC(v.c, gc)
	xproto.FreePixmap(v.c, pix)
	if err != nil {
		return 0
	}
	return cur
}

// setCursor shows the worker's pointer shape as the window's cursor (the
// local pointer is where the worker's is while one controls it).
func (v *x11Viewer) setCursor(c *Cursor) {
	if c.BGRA != nil {
		if cur := v.argbCursor(c.W, c.H, c.HotX, c.HotY, c.BGRA); cur != 0 {
			if v.cursor != 0 {
				xproto.FreeCursor(v.c, v.cursor)
			}
			v.cursor = cur
		}
	}
	use := v.cursor
	if !c.Visible && v.blank != 0 {
		use = v.blank
	}
	xproto.ChangeWindowAttributes(v.c, v.win, xproto.CwCursor, []uint32{uint32(use)})
}
