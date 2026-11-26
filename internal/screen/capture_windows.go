//go:build windows

// Package screen captures this machine's screen and injects input into it
// for Longarm's native remote desktop (docs/remote-desktop.md). This is the
// Windows half of the feasibility spike: DXGI Desktop Duplication through
// raw COM vtables, no cgo (Longarm builds with CGO_ENABLED=0).
package screen

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modD3D11  = windows.NewLazySystemDLL("d3d11.dll")
	modDXGI   = windows.NewLazySystemDLL("dxgi.dll")
	modUser32 = windows.NewLazySystemDLL("user32.dll")

	procD3D11CreateDevice             = modD3D11.NewProc("D3D11CreateDevice")
	procCreateDXGIFactory1            = modDXGI.NewProc("CreateDXGIFactory1")
	procSetProcessDpiAwarenessContext = modUser32.NewProc("SetProcessDpiAwarenessContext")
	procSetThreadDesktop              = modUser32.NewProc("SetThreadDesktop")
	procOpenInputDesktop              = modUser32.NewProc("OpenInputDesktop")
	procCloseDesktop                  = modUser32.NewProc("CloseDesktop")
)

// IIDs from the Windows SDK (dxgi.h, dxgi1_2.h, d3d11.h).
var (
	iidIDXGIFactory1   = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidIDXGIOutput1    = windows.GUID{Data1: 0x00cddea8, Data2: 0x939b, Data3: 0x4b83, Data4: [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidID3D11Texture2D = windows.GUID{Data1: 0x6f15aaf2, Data2: 0xd208, Data3: 0x4e89, Data4: [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

// Vtable slots (the SDK's C interface structs, IUnknown included).
const (
	slotRelease = 2

	slotFactory1EnumAdapters1 = 12
	slotAdapterEnumOutputs    = 7
	slotOutputGetDesc         = 7
	slotOutput1DuplicateOut   = 22

	slotDuplGetDesc          = 7
	slotDuplAcquireNextFrame = 8
	slotDuplGetDirtyRects    = 9
	slotDuplReleaseFrame     = 14

	slotDeviceCreateTexture2D = 5
	slotDeviceGetImmediateCtx = 40
	slotTexture2DGetDesc      = 10
	slotCtxMap                = 14
	slotCtxUnmap              = 15
	slotCtxCopyResource       = 47
)

const (
	dxgiErrorNotFound    = 0x887A0002
	dxgiErrorWaitTimeout = 0x887A0027
	dxgiErrorAccessLost  = 0x887A0026
	d3dDriverTypeUnknown = 0
	d3d11SDKVersion      = 7
	d3d11UsageStaging    = 3
	d3d11CPUAccessRead   = 0x20000
	d3d11MapRead         = 1
	dxgiFormatB8G8R8A8   = 87
	dpiPerMonitorAwareV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	desktopSwitchDesktop = 0x0100
	genericAll           = 0x10000000
)

// ErrAccessLost: the desktop switched (UAC, lock screen), the mode changed
// or another app went full screen; Open the capturer again.
var ErrAccessLost = errors.New("screen: desktop duplication lost (desktop switch or mode change)")

func comCall(obj *comObj, slot int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(obj.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)...)
	return r
}

// comObj is a COM interface pointer's target: its vtable comes first.
type comObj struct{ vtbl *[64]uintptr }

func release(obj *comObj) {
	if obj != nil {
		comCall(obj, slotRelease)
	}
}

func hr(name string, r uintptr) error {
	if int32(r) < 0 {
		return fmt.Errorf("%s: HRESULT 0x%08X", name, uint32(r))
	}
	return nil
}

type rect struct{ Left, Top, Right, Bottom int32 }

// Rect is a changed area of a frame, in the frame's pixels.
type Rect struct{ X, Y, W, H int }

type outputDesc struct {
	DeviceName         [32]uint16
	DesktopCoordinates rect
	AttachedToDesktop  int32
	Rotation           uint32
	Monitor            uintptr
}

type duplDesc struct {
	Width, Height              uint32
	RefreshNum, RefreshDen     uint32
	Format                     uint32
	ScanlineOrdering, Scaling  uint32
	Rotation                   uint32
	DesktopImageInSystemMemory int32
}

type frameInfo struct {
	LastPresentTime         int64
	LastMouseUpdateTime     int64
	AccumulatedFrames       uint32
	RectsCoalesced          int32
	ProtectedContentMasked  int32
	PointerX, PointerY      int32
	PointerVisible          int32
	TotalMetadataBufferSize uint32
	PointerShapeBufferSize  uint32
}

type texture2DDesc struct {
	Width, Height, MipLevels, ArraySize, Format uint32
	SampleCount, SampleQuality                  uint32
	Usage, BindFlags, CPUAccessFlags, MiscFlags uint32
}

type mappedSubresource struct {
	Data       unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

// Display is one monitor attached to the desktop.
type Display struct {
	Index      int
	Name       string
	X, Y, W, H int // desktop coordinates
}

// Capturer duplicates one display. Use it from one goroutine: it locks
// that goroutine to its OS thread (the thread is attached to the input
// desktop).
type Capturer struct {
	Display Display
	W, H    int

	device, ctx, dupl, staging *comObj
	frame                      []byte // BGRA, W*H*4, the last full image
	dirtyBuf                   []rect
}

// SetDPIAware makes the process per-monitor DPI aware, so coordinates are
// physical pixels (call once, early).
func SetDPIAware() {
	if procSetProcessDpiAwarenessContext.Find() == nil {
		procSetProcessDpiAwarenessContext.Call(dpiPerMonitorAwareV2)
	}
}

// attachInputDesktop puts the calling thread on the desktop that gets
// input now (Default, or Winlogon for UAC prompts and the lock screen -
// which only a SYSTEM process in the session may open).
func attachInputDesktop() {
	h, _, _ := procOpenInputDesktop.Call(0, 0, genericAll)
	if h == 0 {
		return
	}
	if r, _, _ := procSetThreadDesktop.Call(h); r == 0 {
		procCloseDesktop.Call(h)
	}
}

// Displays lists the monitors attached to the desktop.
func Displays() ([]Display, error) {
	var out []Display
	err := eachOutput(func(i int, adapter, output *comObj, d outputDesc) bool {
		out = append(out, display(i, d))
		return true
	})
	return out, err
}

func display(i int, d outputDesc) Display {
	r := d.DesktopCoordinates
	return Display{Index: i, Name: windows.UTF16ToString(d.DeviceName[:]),
		X: int(r.Left), Y: int(r.Top), W: int(r.Right - r.Left), H: int(r.Bottom - r.Top)}
}

// eachOutput calls f for every output attached to the desktop, across
// adapters; adapter and output are only valid during the call.
func eachOutput(f func(i int, adapter, output *comObj, d outputDesc) bool) error {
	var factory *comObj
	r, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory)))
	if err := hr("CreateDXGIFactory1", r); err != nil {
		return err
	}
	defer release(factory)
	n := 0
	for a := 0; ; a++ {
		var adapter *comObj
		if r := comCall(factory, slotFactory1EnumAdapters1, uintptr(a), uintptr(unsafe.Pointer(&adapter))); uint32(r) == dxgiErrorNotFound {
			return nil
		} else if err := hr("EnumAdapters1", r); err != nil {
			return err
		}
		for o := 0; ; o++ {
			var output *comObj
			if r := comCall(adapter, slotAdapterEnumOutputs, uintptr(o), uintptr(unsafe.Pointer(&output))); int32(r) < 0 {
				break
			}
			var d outputDesc
			comCall(output, slotOutputGetDesc, uintptr(unsafe.Pointer(&d)))
			more := true
			if d.AttachedToDesktop != 0 {
				more = f(n, adapter, output, d)
				n++
			}
			release(output)
			if !more {
				release(adapter)
				return nil
			}
		}
		release(adapter)
	}
}

// Open starts duplicating display index (0: the first, normally primary).
func Open(index int) (*Capturer, error) {
	runtime.LockOSThread()
	attachInputDesktop()
	c := &Capturer{}
	found := false
	var openErr error
	err := eachOutput(func(i int, adapter, output *comObj, d outputDesc) bool {
		if i != index {
			return true
		}
		found = true
		c.Display = display(i, d)
		openErr = c.open(adapter, output)
		return false
	})
	if err == nil {
		err = openErr
	}
	if err == nil && !found {
		err = fmt.Errorf("screen: no display %d", index)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Capturer) open(adapter, output *comObj) error {
	var level uint32
	r, _, _ := procD3D11CreateDevice.Call(uintptr(unsafe.Pointer(adapter)), d3dDriverTypeUnknown, 0, 0, 0, 0, d3d11SDKVersion,
		uintptr(unsafe.Pointer(&c.device)), uintptr(unsafe.Pointer(&level)), uintptr(unsafe.Pointer(&c.ctx)))
	if err := hr("D3D11CreateDevice", r); err != nil {
		return err
	}
	var out1 *comObj
	if err := hr("QueryInterface(IDXGIOutput1)", comCall(output, 0, uintptr(unsafe.Pointer(&iidIDXGIOutput1)), uintptr(unsafe.Pointer(&out1)))); err != nil {
		return err
	}
	defer release(out1)
	if err := hr("DuplicateOutput", comCall(out1, slotOutput1DuplicateOut, uintptr(unsafe.Pointer(c.device)), uintptr(unsafe.Pointer(&c.dupl)))); err != nil {
		return err
	}
	var dd duplDesc
	comCall(c.dupl, slotDuplGetDesc, uintptr(unsafe.Pointer(&dd)))
	c.W, c.H = int(dd.Width), int(dd.Height)
	td := texture2DDesc{Width: dd.Width, Height: dd.Height, MipLevels: 1, ArraySize: 1, Format: dxgiFormatB8G8R8A8,
		SampleCount: 1, Usage: d3d11UsageStaging, CPUAccessFlags: d3d11CPUAccessRead}
	if err := hr("CreateTexture2D", comCall(c.device, slotDeviceCreateTexture2D, uintptr(unsafe.Pointer(&td)), 0, uintptr(unsafe.Pointer(&c.staging)))); err != nil {
		return err
	}
	c.frame = make([]byte, c.W*c.H*4)
	return nil
}

// Close releases the duplication.
func (c *Capturer) Close() {
	release(c.staging)
	release(c.dupl)
	release(c.ctx)
	release(c.device)
	c.staging, c.dupl, c.ctx, c.device = nil, nil, nil, nil
	runtime.UnlockOSThread()
}

// Frame is the screen image (BGRA, stride W*4); valid until the next Next.
func (c *Capturer) Frame() []byte { return c.frame }

// Next waits up to timeoutMs for the screen to change and updates Frame.
// It returns the changed rectangles (none on a timeout: nothing changed)
// and the pointer position. ErrAccessLost: Open again.
func (c *Capturer) Next(timeoutMs int) (dirty []Rect, err error) {
	var fi frameInfo
	var res *comObj
	r := comCall(c.dupl, slotDuplAcquireNextFrame, uintptr(timeoutMs), uintptr(unsafe.Pointer(&fi)), uintptr(unsafe.Pointer(&res)))
	switch uint32(r) {
	case dxgiErrorWaitTimeout:
		return nil, nil
	case dxgiErrorAccessLost:
		return nil, ErrAccessLost
	}
	if err := hr("AcquireNextFrame", r); err != nil {
		return nil, err
	}
	defer comCall(c.dupl, slotDuplReleaseFrame)
	defer release(res)
	if fi.LastPresentTime == 0 {
		return nil, nil // only the pointer moved
	}
	// Dirty rects (move rects are reported as dirty too: the spike copies
	// the whole staging texture anyway).
	if c.dirtyBuf == nil {
		c.dirtyBuf = make([]rect, 256)
	}
	var need uint32
	sz := uint32(len(c.dirtyBuf)) * uint32(unsafe.Sizeof(rect{}))
	r = comCall(c.dupl, slotDuplGetDirtyRects, uintptr(sz), uintptr(unsafe.Pointer(&c.dirtyBuf[0])), uintptr(unsafe.Pointer(&need)))
	full := int32(r) < 0 // buffer too small, or anything else: send everything
	var tex *comObj
	if err := hr("QueryInterface(ID3D11Texture2D)", comCall(res, 0, uintptr(unsafe.Pointer(&iidID3D11Texture2D)), uintptr(unsafe.Pointer(&tex)))); err != nil {
		return nil, err
	}
	comCall(c.ctx, slotCtxCopyResource, uintptr(unsafe.Pointer(c.staging)), uintptr(unsafe.Pointer(tex)))
	release(tex)
	var m mappedSubresource
	if err := hr("Map", comCall(c.ctx, slotCtxMap, uintptr(unsafe.Pointer(c.staging)), 0, d3d11MapRead, 0, uintptr(unsafe.Pointer(&m)))); err != nil {
		return nil, err
	}
	row := c.W * 4
	src := unsafe.Slice((*byte)(m.Data), int(m.RowPitch)*(c.H-1)+row)
	for y := 0; y < c.H; y++ {
		copy(c.frame[y*row:(y+1)*row], src[y*int(m.RowPitch):])
	}
	comCall(c.ctx, slotCtxUnmap, uintptr(unsafe.Pointer(c.staging)), 0)
	if full {
		return []Rect{{0, 0, c.W, c.H}}, nil
	}
	for _, d := range c.dirtyBuf[:need/uint32(unsafe.Sizeof(rect{}))] {
		dirty = append(dirty, Rect{int(d.Left), int(d.Top), int(d.Right - d.Left), int(d.Bottom - d.Top)})
	}
	return dirty, nil
}
