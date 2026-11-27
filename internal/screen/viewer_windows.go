//go:build windows

package screen

import (
	"log/slog"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modGDI32               = windows.NewLazySystemDLL("gdi32.dll")
	procStretchDIBits      = modGDI32.NewProc("StretchDIBits")
	procSetStretchBltMode  = modGDI32.NewProc("SetStretchBltMode")
	procCreateBitmap       = modGDI32.NewProc("CreateBitmap")
	procDeleteObject       = modGDI32.NewProc("DeleteObject")
	procPatBlt             = modGDI32.NewProc("PatBlt")
	procGetDC              = modUser32.NewProc("GetDC")
	procReleaseDC          = modUser32.NewProc("ReleaseDC")
	procCreateIconIndirect = modUser32.NewProc("CreateIconIndirect")
	procDestroyIcon        = modUser32.NewProc("DestroyIcon")
)

type bitmapInfoHeader struct {
	Size                         uint32
	Width, Height                int32
	Planes, BitCount             uint16
	Compression, SizeImage       uint32
	XPelsPerMeter, YPelsPerMeter int32
	ClrUsed, ClrImportant        uint32
}

type iconInfo struct {
	Icon               int32
	XHotspot, YHotspot uint32
	Mask, Color        uintptr
}

// gdiCanvas is the viewer's software path: a BGRA image in memory, drawn
// into the window with StretchDIBits (letterboxed). It works without a
// GPU (virtual machines, CI) and for JPEG tiles.
type gdiCanvas struct {
	hwnd windows.HWND
	w, h int
	img  []byte
	dest rect
}

func (c *gdiCanvas) paint() {
	if c.img == nil {
		return
	}
	cw, ch := clientSize(c.hwnd)
	if cw == 0 || ch == 0 {
		return
	}
	w, h := cw, c.h*cw/c.w
	if h > ch {
		w, h = c.w*ch/c.h, ch
	}
	c.dest = rect{int32((cw - w) / 2), int32((ch - h) / 2), int32((cw-w)/2 + w), int32((ch-h)/2 + h)}
	dc, _, _ := procGetDC.Call(uintptr(c.hwnd))
	defer procReleaseDC.Call(uintptr(c.hwnd), dc)
	const blackness, halftone, srcCopy = 0x00000042, 4, 0x00CC0020
	// The bars around the picture.
	procPatBlt.Call(dc, 0, 0, uintptr(cw), uintptr(c.dest.Top), blackness)
	procPatBlt.Call(dc, 0, uintptr(c.dest.Bottom), uintptr(cw), uintptr(ch), blackness)
	procPatBlt.Call(dc, 0, 0, uintptr(c.dest.Left), uintptr(ch), blackness)
	procPatBlt.Call(dc, uintptr(c.dest.Right), 0, uintptr(cw), uintptr(ch), blackness)
	procSetStretchBltMode.Call(dc, halftone)
	bi := bitmapInfoHeader{Width: int32(c.w), Height: -int32(c.h), Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	procStretchDIBits.Call(dc, uintptr(c.dest.Left), uintptr(c.dest.Top), uintptr(w), uintptr(h),
		0, 0, uintptr(c.w), uintptr(c.h), uintptr(unsafe.Pointer(&c.img[0])), uintptr(unsafe.Pointer(&bi)), 0, srcCopy)
}

// cursorFrom makes a Windows cursor of a BGRA pointer shape.
func cursorFrom(cur *Cursor) uintptr {
	if cur.W <= 0 || cur.H <= 0 || len(cur.BGRA) < cur.W*cur.H*4 {
		return 0
	}
	color, _, _ := procCreateBitmap.Call(uintptr(cur.W), uintptr(cur.H), 1, 32, uintptr(unsafe.Pointer(&cur.BGRA[0])))
	mask, _, _ := procCreateBitmap.Call(uintptr(cur.W), uintptr(cur.H), 1, 1, 0)
	defer procDeleteObject.Call(color)
	defer procDeleteObject.Call(mask)
	ii := iconInfo{XHotspot: uint32(cur.HotX), YHotspot: uint32(cur.HotY), Mask: mask, Color: color}
	h, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	return h
}

func runViewer(o ViewerOptions) error {
	SetDPIAware()
	win, err := NewWindow(o.Title+fullscreenHint, 1600, 940, o.Input != nil)
	if err != nil {
		return err
	}
	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ev)
	var mu sync.Mutex
	var queue []VideoPacket
	ended := false
	go func() {
		for p := range o.Packets {
			mu.Lock()
			queue = append(queue, p)
			mu.Unlock()
			windows.SetEvent(ev)
		}
		mu.Lock()
		ended = true
		mu.Unlock()
		windows.SetEvent(ev)
	}()
	stop := make(chan struct{})
	defer close(stop)
	go func() { // the title's status, once a second
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				windows.SetEvent(ev)
			case <-stop:
				return
			}
		}
	}()

	var (
		cfg      VideoPacket
		gpu      *Presenter
		sw       VideoDecoder
		canvas   = &gdiCanvas{hwnd: win.HWND}
		cursor   uintptr
		lastTick time.Time
	)
	closeDecoders := func() {
		if gpu != nil {
			gpu.Close()
			gpu = nil
		}
		if sw != nil {
			sw.Close()
			sw = nil
		}
	}
	defer closeDecoders()
	defer func() {
		if cursor != 0 {
			procDestroyIcon.Call(cursor)
		}
	}()
	dest := func() (int, int, int, int) {
		if gpu != nil {
			return gpu.Dest()
		}
		d := canvas.dest
		return int(d.Left), int(d.Top), int(d.Right - d.Left), int(d.Bottom - d.Top)
	}
	win.OnResize = func() {
		if gpu != nil {
			if err := gpu.Resize(); err != nil {
				slog.Debug("desktop viewer: resize", "err", err)
			}
		} else {
			canvas.paint()
		}
	}
	win.OnCursor = func() uintptr { return cursor }
	if o.Input != nil {
		toRemote := func(x, y int) (int, int, bool) {
			dx, dy, dw, dh := dest()
			if dw <= 0 || dh <= 0 || cfg.Width == 0 {
				return 0, 0, false
			}
			fx, fy := (x-dx)*cfg.Width/dw, (y-dy)*cfg.Height/dh
			return min(max(fx, 0), cfg.Width-1), min(max(fy, 0), cfg.Height-1), true
		}
		win.OnMouse = func(kind, x, y, button, wdx, wdy int) {
			switch kind {
			case MouseWheeled:
				o.Input(InputEvent{Mouse: &MouseEvent{WheelX: wdx, WheelY: wdy}})
			default:
				fx, fy, ok := toRemote(x, y)
				if !ok {
					return
				}
				m := &MouseEvent{X: fx, Y: fy}
				if kind != MouseMoved {
					m.Button, m.Down = button, kind == MouseDown
				}
				o.Input(InputEvent{Mouse: m})
			}
		}
		win.OnKey = func(scan uint16, down bool) { o.Input(InputEvent{Key: &KeyEvent{Scan: scan, Down: down}}) }
		win.OnSAS = func() { o.Input(InputEvent{SAS: true}) }
	}

	handle := func(p VideoPacket) {
		if p.Cursor != nil {
			if p.Cursor.BGRA != nil {
				if cursor != 0 {
					procDestroyIcon.Call(cursor)
				}
				cursor = cursorFrom(p.Cursor)
			}
			return
		}
		if p.Config {
			closeDecoders()
			cfg = p
			canvas.w, canvas.h, canvas.img = p.Width, p.Height, make([]byte, p.Width*p.Height*4)
			if p.Codec == "h264" {
				if gpu, err = NewPresenter(win.HWND, p.Width, p.Height); err != nil {
					slog.Debug("desktop viewer: no GPU decoding, decoding in software", "err", err)
					gpu = nil
					if sw, err = NewH264Decoder(p.Width, p.Height); err != nil {
						slog.Error("desktop viewer: no H.264 decoder", "err", err)
					}
				}
			}
			return
		}
		shown := false
		switch {
		case cfg.Codec == "h264" && gpu != nil:
			ok, err := gpu.Decode(p.Data)
			if err != nil {
				slog.Debug("desktop viewer: decode", "err", err)
				if o.NeedKeyframe != nil {
					o.NeedKeyframe()
				}
			}
			shown = ok
		case cfg.Codec == "h264" && sw != nil:
			img, _, _, err := sw.Decode(p.Data)
			if err != nil {
				slog.Debug("desktop viewer: decode", "err", err)
				if o.NeedKeyframe != nil {
					o.NeedKeyframe()
				}
			}
			if img != nil {
				copy(canvas.img, img)
				canvas.paint()
				shown = true
			}
		case cfg.Codec == "jpeg":
			DrawTiles(canvas.img, canvas.w, canvas.h, p.Tiles)
			canvas.paint()
			shown = true
		}
		if shown && o.Shown != nil {
			o.Shown(p.Seq)
		}
	}

	win.Run(ev, func() {
		mu.Lock()
		q := queue
		queue = nil
		done := ended
		mu.Unlock()
		for _, p := range q {
			handle(p)
		}
		if o.Status != nil && time.Since(lastTick) >= time.Second {
			lastTick = time.Now()
			if s := o.Status(); s != "" {
				win.SetTitle(o.Title + " - " + s + fullscreenHint)
			}
		}
		if done {
			win.Close()
		}
	})
	theWindow = nil
	return nil
}

// The viewer window exists on Windows.
const canView = true

// fullscreenHint: the title says how to get in and out of full screen (the
// maximize button goes in too; out is the keys).
const fullscreenHint = "   (Ctrl+Alt+Enter: full screen)"
