//go:build linux

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// pattern shows the remote desktop test's picture (e2e/desktop_test.go)
// over the whole X screen for secs seconds: red, green, blue and white
// quadrants and a black square in the middle that turns yellow when
// clicked. It writes "ready W H" to stdout once shown and every click
// ("click X Y") and character ("char C") to logPath. The window is
// override-redirect: under Xvfb there is no window manager to place it.
func pattern(logPath string, secs int) {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "pattern:", err)
		os.Exit(1)
	}
	log, err := os.Create(logPath)
	if err != nil {
		fail(err)
	}
	defer log.Close()
	c, err := xgb.NewConn()
	if err != nil {
		fail(err)
	}
	defer c.Close()
	setup := xproto.Setup(c)
	scr := setup.DefaultScreen(c)
	w, h := int16(scr.WidthInPixels), int16(scr.HeightInPixels)
	id, _ := c.NewId()
	win := xproto.Window(id)
	if err := xproto.CreateWindowChecked(c, scr.RootDepth, win, scr.Root, 0, 0, uint16(w), uint16(h), 0,
		xproto.WindowClassInputOutput, scr.RootVisual, xproto.CwBackPixel|xproto.CwOverrideRedirect|xproto.CwEventMask,
		[]uint32{0, 1, xproto.EventMaskExposure | xproto.EventMaskButtonPress | xproto.EventMaskKeyPress}).Check(); err != nil {
		fail(err)
	}
	gcs := map[uint32]xproto.Gcontext{}
	fill := func(rgb uint32, x, y, rw, rh int16) {
		gc, ok := gcs[rgb]
		if !ok {
			id, _ := c.NewId()
			gc = xproto.Gcontext(id)
			xproto.CreateGC(c, gc, xproto.Drawable(win), xproto.GcForeground, []uint32{rgb}) // TrueColor: the pixel is the colour
			gcs[rgb] = gc
		}
		xproto.PolyFillRectangle(c, xproto.Drawable(win), gc, []xproto.Rectangle{{X: x, Y: y, Width: uint16(rw), Height: uint16(rh)}})
	}
	clicked := false
	paint := func() {
		fill(0xFF0000, 0, 0, w/2, h/2)
		fill(0x00FF00, w/2, 0, w-w/2, h/2)
		fill(0x0000FF, 0, h/2, w/2, h-h/2)
		fill(0xFFFFFF, w/2, h/2, w-w/2, h-h/2)
		sq := uint32(0)
		if clicked {
			sq = 0xFFFF00
		}
		fill(sq, w/2-60, h/2-60, 120, 120)
		c.Sync()
	}
	var keysyms []xproto.Keysym
	per := 0
	keymap := func() {
		n := byte(setup.MaxKeycode - setup.MinKeycode + 1)
		if r, err := xproto.GetKeyboardMapping(c, setup.MinKeycode, n).Reply(); err == nil {
			keysyms, per = r.Keysyms, int(r.KeysymsPerKeycode)
		}
	}
	keymap()
	xproto.MapWindow(c, win)
	xproto.SetInputFocus(c, xproto.InputFocusPointerRoot, win, xproto.TimeCurrentTime)
	paint()
	time.AfterFunc(500*time.Millisecond, func() { fmt.Printf("ready %d %d\n", w, h) })
	time.AfterFunc(time.Duration(secs)*time.Second, func() { os.Exit(0) })
	for {
		ev, err := c.WaitForEvent()
		if ev == nil && err == nil {
			return
		}
		switch e := ev.(type) {
		case xproto.ExposeEvent:
			paint()
		case xproto.ButtonPressEvent:
			if e.Detail == 1 {
				fmt.Fprintf(log, "click %d %d\n", e.EventX, e.EventY)
				log.Sync()
				clicked = true
				paint()
			}
		case xproto.MappingNotifyEvent:
			keymap()
		case xproto.KeyPressEvent:
			// What XLookupString does for ASCII: the keysym of the key's
			// level (Shift: the second), printable as is.
			i := int(e.Detail-setup.MinKeycode) * per
			if per == 0 || i < 0 || i+1 >= len(keysyms) {
				continue
			}
			ks := keysyms[i]
			if e.State&xproto.ModMaskShift != 0 && keysyms[i+1] != 0 {
				ks = keysyms[i+1]
			}
			if ks >= 0x20 && ks <= 0x7E {
				fmt.Fprintf(log, "char %c\n", rune(ks))
				log.Sync()
			}
		}
	}
}
