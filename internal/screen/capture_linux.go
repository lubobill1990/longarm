//go:build linux

package screen

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
	"golang.org/x/sys/unix"
)

// x11Source captures a monitor of the X screen: MIT-SHM when the server
// shares memory with us (a local display), else GetImage; DAMAGE says
// when something changed, so an idle screen costs a pointer query every
// pollEvery and nothing else.
type x11Source struct {
	c    *xgb.Conn
	root xproto.Window
	d    Display

	seg   shm.Seg // 0: no MIT-SHM
	mem   []byte
	frame []byte

	dmg      damage.Damage // 0: no DAMAGE, frames are compared
	prev     []byte
	xfixes   bool
	cursor   Cursor
	wake     chan struct{}
	dirty    atomic.Bool
	newShape atomic.Bool
	lost     atomic.Bool
	closed   chan struct{}
}

const pollEvery = 16 * time.Millisecond

func openSource(display int) (Source, error) {
	c, err := xConnect()
	if err != nil {
		return nil, err
	}
	s, err := newX11Source(c, display)
	if err != nil {
		c.Close()
		return nil, err
	}
	return s, nil
}

func newX11Source(c *xgb.Conn, display int) (*x11Source, error) {
	scr := xproto.Setup(c).DefaultScreen(c)
	if err := xFormat(c, scr); err != nil {
		return nil, err
	}
	ds := xDisplays(c)
	if display < 0 || display >= len(ds) {
		return nil, fmt.Errorf("no display %d (there are %d)", display, len(ds))
	}
	s := &x11Source{c: c, root: scr.Root, d: ds[display], wake: make(chan struct{}, 1), closed: make(chan struct{})}
	s.dirty.Store(true) // the first Next gives a picture
	s.newShape.Store(true)
	s.openShm()
	if xfixes.Init(c) == nil {
		if _, err := xfixes.QueryVersion(c, 5, 0).Reply(); err == nil {
			s.xfixes = true
			xfixes.SelectCursorInput(c, s.root, xfixes.CursorNotifyMaskDisplayCursor)
		}
	}
	if s.xfixes && damage.Init(c) == nil {
		if _, err := damage.QueryVersion(c, 1, 1).Reply(); err == nil {
			if id, err := c.NewId(); err == nil {
				s.dmg = damage.Damage(id)
				if damage.CreateChecked(c, s.dmg, xproto.Drawable(s.root), damage.ReportLevelNonEmpty).Check() != nil {
					s.dmg = 0
				}
			}
		}
	}
	// A size change ends the source (rdesk opens a new one).
	xproto.ChangeWindowAttributes(c, s.root, xproto.CwEventMask, []uint32{xproto.EventMaskStructureNotify})
	go s.events(scr.WidthInPixels, scr.HeightInPixels)
	return s, nil
}

// openShm shares the frame's memory with the X server; it stays off
// (GetImage) for a remote display or a server without MIT-SHM.
func (s *x11Source) openShm() {
	s.seg, s.mem = xShm(s.c, s.d.W*s.d.H*4)
}

// events reads the connection's events until it closes.
func (s *x11Source) events(w0, h0 uint16) {
	defer close(s.closed)
	for {
		ev, err := s.c.WaitForEvent()
		if ev == nil && err == nil {
			s.lost.Store(true)
			signal(s.wake)
			return
		}
		switch e := ev.(type) {
		case damage.NotifyEvent:
			s.dirty.Store(true)
		case xfixes.CursorNotifyEvent:
			s.newShape.Store(true)
		case xproto.ConfigureNotifyEvent:
			if e.Window == s.root && (e.Width != w0 || e.Height != h0) {
				s.lost.Store(true)
			}
		default:
			continue
		}
		signal(s.wake)
	}
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (s *x11Source) Display() Display { return s.d }
func (s *x11Source) Size() (int, int) { return s.d.W, s.d.H }
func (s *x11Source) Frame() []byte    { return s.frame }

func (s *x11Source) Cursor() Cursor {
	c := s.cursor
	s.cursor.BGRA = nil // handed out once
	return c
}

func (s *x11Source) Next(timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		if s.lost.Load() {
			return false, ErrAccessLost
		}
		if s.dmg == 0 || s.dirty.Swap(false) {
			if s.dmg != 0 {
				// Re-arm before capturing: what changes from now on
				// reports again.
				damage.Subtract(s.c, s.dmg, 0, 0)
			}
			if err := s.grab(); err != nil {
				return false, err
			}
			if s.dmg != 0 || s.prev == nil || !bytes.Equal(s.prev, s.frame) {
				if s.dmg == 0 {
					s.prev = append(s.prev[:0], s.frame...)
				}
				s.pointer()
				return true, nil
			}
		}
		if s.pointer() {
			return false, nil // the pointer moved: rdesk sends it now
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-s.wake:
		case <-tick.C:
		}
	}
}

// pointer updates the cursor; it says whether it moved or changed shape.
func (s *x11Source) pointer() bool {
	if s.xfixes && s.newShape.Swap(false) {
		r, err := xfixes.GetCursorImage(s.c).Reply()
		if err == nil {
			s.cursor.ShapeID++
			s.cursor.W, s.cursor.H = int(r.Width), int(r.Height)
			s.cursor.HotX, s.cursor.HotY = int(r.Xhot), int(r.Yhot)
			s.cursor.BGRA = straightBGRA(r.CursorImage, int(r.Width)*int(r.Height))
			s.cursor.X, s.cursor.Y, s.cursor.Visible = int(r.X)-s.d.X, int(r.Y)-s.d.Y, true
			return true
		}
	}
	r, err := xproto.QueryPointer(s.c, s.root).Reply()
	if err != nil {
		return false
	}
	x, y := int(r.RootX)-s.d.X, int(r.RootY)-s.d.Y
	if x == s.cursor.X && y == s.cursor.Y && s.cursor.Visible {
		return false
	}
	s.cursor.X, s.cursor.Y, s.cursor.Visible = x, y, true
	return true
}

// straightBGRA turns XFixes' premultiplied ARGB words into straight BGRA.
func straightBGRA(argb []uint32, n int) []byte {
	out := make([]byte, n*4)
	for i := 0; i < n && i < len(argb); i++ {
		p := argb[i]
		a := p >> 24
		r, g, b := p>>16&0xFF, p>>8&0xFF, p&0xFF
		if a != 0 && a != 255 {
			r, g, b = min(r*255/a, 255), min(g*255/a, 255), min(b*255/a, 255)
		}
		out[i*4], out[i*4+1], out[i*4+2], out[i*4+3] = byte(b), byte(g), byte(r), byte(a)
	}
	return out
}

func (s *x11Source) grab() error {
	d := s.d
	if s.seg != 0 {
		_, err := shm.GetImage(s.c, xproto.Drawable(s.root), int16(d.X), int16(d.Y), uint16(d.W), uint16(d.H),
			0xFFFFFFFF, xproto.ImageFormatZPixmap, s.seg, 0).Reply()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrAccessLost, err)
		}
		s.frame = s.mem[:d.W*d.H*4]
		return nil
	}
	r, err := xproto.GetImage(s.c, xproto.ImageFormatZPixmap, xproto.Drawable(s.root), int16(d.X), int16(d.Y),
		uint16(d.W), uint16(d.H), 0xFFFFFFFF).Reply()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAccessLost, err)
	}
	if len(r.Data) < d.W*d.H*4 {
		return fmt.Errorf("%w: short image", ErrAccessLost)
	}
	s.frame = r.Data[:d.W*d.H*4]
	return nil
}

func (s *x11Source) Close() {
	if s.dmg != 0 {
		damage.Destroy(s.c, s.dmg)
	}
	if s.seg != 0 {
		shm.Detach(s.c, s.seg)
	}
	s.c.Close()
	<-s.closed
	if s.mem != nil {
		unix.SysvShmDetach(s.mem)
	}
}
