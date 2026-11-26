//go:build windows

package screen

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The viewer's half: H.264 decoded on the GPU (Media Foundation's decoder
// with a DXGI device manager: DXVA, NV12 textures that never leave video
// memory) and drawn into a window's flip-model swap chain by the D3D11
// video processor, which converts NV12 to RGB and scales in one step. No
// shaders, no cgo.

var (
	procMFCreateDXGIDeviceManager = modMFPlat.NewProc("MFCreateDXGIDeviceManager")

	iidIDXGIDevice          = guid(0x54ec77fa, 0x1377, 0x44e6, 0x8c, 0x32, 0x88, 0xfd, 0x5f, 0x44, 0xc8, 0x4c)
	iidIDXGIFactory2        = guid(0x50c83a1c, 0xe072, 0x4c48, 0x87, 0xb0, 0x36, 0x30, 0xfa, 0x36, 0xa6, 0xd0)
	iidID3D11VideoDevice    = guid(0x10ec4d5b, 0x975a, 0x4689, 0xb9, 0xe4, 0xd0, 0xaa, 0xc3, 0x0f, 0xe3, 0x33)
	iidID3D11VideoContext   = guid(0x61f21c45, 0x3c0e, 0x4a74, 0x9c, 0xea, 0x67, 0x10, 0x0d, 0x9a, 0xd5, 0xe4)
	iidID3D10Multithread    = guid(0x9b7e4e00, 0x342c, 0x4106, 0xa1, 0x9f, 0x4f, 0x27, 0x04, 0xf6, 0x89, 0xf0)
	iidIMFDXGIBuffer        = guid(0xe7174cfa, 0x1c9e, 0x48b1, 0x88, 0x66, 0x62, 0x62, 0x26, 0xbf, 0xc2, 0x58)
	mftCategoryVideoDecoder = guid(0xd6c02d4b, 0x6833, 0x45b4, 0x97, 0x1a, 0x05, 0xa4, 0xb0, 0x4b, 0xab, 0x91)
)

const (
	d3dDriverTypeHardware   = 1
	d3d11CreateVideoSupport = 0x800
	d3d11CreateBGRASupport  = 0x20

	dxgiUsageRenderTarget = 0x20
	dxgiSwapFlipDiscard   = 4
	dxgiScalingStretch    = 0

	slotDXGIObjectGetParent  = 6
	slotDXGIDeviceGetAdapter = 7
	slotFactory2SwapForHwnd  = 15
	slotSwapPresent          = 8
	slotSwapGetBuffer        = 9
	slotSwapResizeBuffers    = 13

	slotVDevCreateVP         = 4
	slotVDevCreateInputView  = 8
	slotVDevCreateOutputView = 9
	slotVDevCreateVPEnum     = 10

	slotVCtxSetOutputTargetRect = 13
	slotVCtxSetOutputBgColor    = 14
	slotVCtxSetOutputColorSpace = 15
	slotVCtxSetStreamFrameFmt   = 27
	slotVCtxSetStreamColorSpace = 28
	slotVCtxSetStreamSourceRect = 30
	slotVCtxSetStreamDestRect   = 31
	slotVCtxSetStreamAutoProc   = 37
	slotVCtxBlt                 = 53

	slotMultithreadSetProtected = 5
	slotDevMgrResetDevice       = 7
	slotDXGIBufGetResource      = 3
	slotDXGIBufGetSubresource   = 4
	slotSampleGetBufferByIndex  = 40
	slotAttrGetGUID             = 10
	slotAttrGetUINT64           = 8
	slotMFTGetOutputCurrentType = 18

	mftMsgSetD3DManager = 2
)

type swapChainDesc1 struct {
	Width, Height, Format      uint32
	Stereo                     int32
	SampleCount, SampleQuality uint32
	BufferUsage, BufferCount   uint32
	Scaling, SwapEffect, Alpha uint32
	Flags                      uint32
}

type vpContentDesc struct {
	InputFrameFormat          uint32
	InRateNum, InRateDen      uint32
	InputWidth, InputHeight   uint32
	OutRateNum, OutRateDen    uint32
	OutputWidth, OutputHeight uint32
	Usage                     uint32
}

type vpInputViewDesc struct {
	FourCC, ViewDimension, MipSlice, ArraySlice uint32
}

type vpOutputViewDesc struct {
	ViewDimension, MipSlice, FirstArraySlice, ArraySize uint32
}

type vpStream struct {
	Enable            int32
	OutputIndex       uint32
	InputFrameOrField uint32
	PastFrames        uint32
	FutureFrames      uint32
	_                 uint32
	PastSurfaces      uintptr
	InputSurface      *comObj
	FutureSurfaces    uintptr
	PastSurfacesR     uintptr
	InputSurfaceR     uintptr
	FutureSurfacesR   uintptr
}

// Presenter decodes H.264 access units and shows them in a window,
// letterboxed. Use it from the window's thread only.
type Presenter struct {
	hwnd                    windows.HWND
	device, ctx, vdev, vctx *comObj
	swap, mgr, dec          *comObj
	vpEnum, vp, outView     *comObj
	last                    *comObj // the last decoded sample (shown again after a resize)

	frameW, frameH int // the picture (the encoder's size)
	texW, texH     int // the decoder's surfaces (can be padded)
	bufW, bufH     int // the swap chain
	dest           rect

	// Decoded counts the frames out of the decoder.
	Decoded int
	// Snapshot, when set, gets the next presented image (then is cleared).
	Snapshot func(bgra []byte, w, h int)
}

// NewPresenter sets up the GPU, the decoder and a swap chain for hwnd.
func NewPresenter(hwnd windows.HWND, frameW, frameH int) (*Presenter, error) {
	procCoInitializeEx.Call(0, 0)
	if err := mfStartup(); err != nil {
		return nil, err
	}
	p := &Presenter{hwnd: hwnd, frameW: frameW, frameH: frameH}
	if err := p.init(); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func (p *Presenter) init() error {
	var level uint32
	r, _, _ := procD3D11CreateDevice.Call(0, d3dDriverTypeHardware, 0, d3d11CreateVideoSupport|d3d11CreateBGRASupport, 0, 0, d3d11SDKVersion,
		uintptr(unsafe.Pointer(&p.device)), uintptr(unsafe.Pointer(&level)), uintptr(unsafe.Pointer(&p.ctx)))
	if err := hr("D3D11CreateDevice", r); err != nil {
		return err
	}
	// The decoder uses the device from its own threads.
	var mt *comObj
	if comCall(p.device, 0, uintptr(unsafe.Pointer(&iidID3D10Multithread)), uintptr(unsafe.Pointer(&mt))) == 0 {
		comCall(mt, slotMultithreadSetProtected, 1)
		release(mt)
	}
	if err := hr("QueryInterface(ID3D11VideoDevice)", comCall(p.device, 0, uintptr(unsafe.Pointer(&iidID3D11VideoDevice)), uintptr(unsafe.Pointer(&p.vdev)))); err != nil {
		return err
	}
	if err := hr("QueryInterface(ID3D11VideoContext)", comCall(p.ctx, 0, uintptr(unsafe.Pointer(&iidID3D11VideoContext)), uintptr(unsafe.Pointer(&p.vctx)))); err != nil {
		return err
	}
	if err := p.createSwapChain(); err != nil {
		return err
	}
	var token uint32
	r, _, _ = procMFCreateDXGIDeviceManager.Call(uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&p.mgr)))
	if err := hr("MFCreateDXGIDeviceManager", r); err != nil {
		return err
	}
	if err := hr("ResetDevice", comCall(p.mgr, slotDevMgrResetDevice, uintptr(unsafe.Pointer(p.device)), uintptr(token))); err != nil {
		return err
	}
	return p.openDecoder()
}

func (p *Presenter) createSwapChain() error {
	var dxgiDev, adapter, factory *comObj
	defer func() { release(factory); release(adapter); release(dxgiDev) }()
	if err := hr("QueryInterface(IDXGIDevice)", comCall(p.device, 0, uintptr(unsafe.Pointer(&iidIDXGIDevice)), uintptr(unsafe.Pointer(&dxgiDev)))); err != nil {
		return err
	}
	if err := hr("GetAdapter", comCall(dxgiDev, slotDXGIDeviceGetAdapter, uintptr(unsafe.Pointer(&adapter)))); err != nil {
		return err
	}
	if err := hr("GetParent(IDXGIFactory2)", comCall(adapter, slotDXGIObjectGetParent, uintptr(unsafe.Pointer(&iidIDXGIFactory2)), uintptr(unsafe.Pointer(&factory)))); err != nil {
		return err
	}
	p.bufW, p.bufH = clientSize(p.hwnd)
	d := swapChainDesc1{Width: uint32(p.bufW), Height: uint32(p.bufH), Format: dxgiFormatB8G8R8A8, SampleCount: 1,
		BufferUsage: dxgiUsageRenderTarget, BufferCount: 2, Scaling: dxgiScalingStretch, SwapEffect: dxgiSwapFlipDiscard}
	return hr("CreateSwapChainForHwnd", comCall(factory, slotFactory2SwapForHwnd, uintptr(unsafe.Pointer(p.device)), uintptr(p.hwnd),
		uintptr(unsafe.Pointer(&d)), 0, 0, uintptr(unsafe.Pointer(&p.swap))))
}

func (p *Presenter) openDecoder() error {
	in := mftRegisterTypeInfo{mfMediaTypeVideo, mfVideoFormatH264}
	out := mftRegisterTypeInfo{mfMediaTypeVideo, mfVideoFormatNV12}
	var acts **comObj
	var n uint32
	r, _, _ := procMFTEnumEx.Call(uintptr(unsafe.Pointer(&mftCategoryVideoDecoder)), mftEnumSyncMFT|mftEnumSortAndFilter,
		uintptr(unsafe.Pointer(&in)), uintptr(unsafe.Pointer(&out)), uintptr(unsafe.Pointer(&acts)), uintptr(unsafe.Pointer(&n)))
	if err := hr("MFTEnumEx(decoder)", r); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("no H.264 decoder")
	}
	list := unsafe.Slice(acts, n)
	defer func() {
		for _, a := range list {
			release(a)
		}
		procCoTaskMemFree.Call(uintptr(unsafe.Pointer(acts)))
	}()
	if err := hr("ActivateObject(decoder)", comCall(list[0], slotActivateObj, uintptr(unsafe.Pointer(&iidIMFTransform)), uintptr(unsafe.Pointer(&p.dec)))); err != nil {
		return err
	}
	var attrs *comObj
	if comCall(p.dec, slotMFTGetAttributes, uintptr(unsafe.Pointer(&attrs))) == 0 && attrs != nil {
		comCall(attrs, slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfLowLatency)), 1) // output each frame at once
		release(attrs)
	}
	if err := hr("SET_D3D_MANAGER", comCall(p.dec, slotMFTProcessMessage, mftMsgSetD3DManager, uintptr(unsafe.Pointer(p.mgr)))); err != nil {
		return fmt.Errorf("the decoder cannot decode on the GPU: %w", err)
	}
	t, err := mediaType(func(t *comObj) {
		comCall(t, slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTMajorType)), uintptr(unsafe.Pointer(&mfMediaTypeVideo)))
		comCall(t, slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&mfVideoFormatH264)))
		comCall(t, slotAttrSetUINT64, uintptr(unsafe.Pointer(&mfMTFrameSize)), uintptr(uint64(p.frameW)<<32|uint64(p.frameH)))
	})
	if err != nil {
		return err
	}
	defer release(t)
	if err := hr("SetInputType(decoder)", comCall(p.dec, slotMFTSetInputType, 0, uintptr(unsafe.Pointer(t)), 0)); err != nil {
		return err
	}
	if err := p.pickNV12(); err != nil {
		return err
	}
	comCall(p.dec, slotMFTProcessMessage, mftMsgBeginStreaming, 0)
	comCall(p.dec, slotMFTProcessMessage, mftMsgStartOfStream, 0)
	return nil
}

// pickNV12 sets the decoder's NV12 output type and reads its surface size.
func (p *Presenter) pickNV12() error {
	for i := 0; ; i++ {
		var t *comObj
		if r := comCall(p.dec, slotMFTGetOutputAvailType, 0, uintptr(i), uintptr(unsafe.Pointer(&t))); int32(r) < 0 {
			return fmt.Errorf("the decoder offers no NV12 output: HRESULT 0x%08X", uint32(r))
		}
		var sub windows.GUID
		comCall(t, slotAttrGetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&sub)))
		if sub != mfVideoFormatNV12 {
			release(t)
			continue
		}
		r := comCall(p.dec, slotMFTSetOutputType, 0, uintptr(unsafe.Pointer(t)), 0)
		var size uint64
		comCall(t, slotAttrGetUINT64, uintptr(unsafe.Pointer(&mfMTFrameSize)), uintptr(unsafe.Pointer(&size)))
		release(t)
		if size != 0 {
			p.texW, p.texH = int(size>>32), int(uint32(size))
		}
		return hr("SetOutputType(decoder)", r)
	}
}

// Decode feeds one access unit and shows what comes out; shown is false
// when the decoder needed more input first.
func (p *Presenter) Decode(au []byte) (shown bool, err error) {
	s, err := newSample(len(au))
	if err != nil {
		return false, err
	}
	var b *comObj
	comCall(s, slotSampleContiguousBuf, uintptr(unsafe.Pointer(&b)))
	var ptr unsafe.Pointer
	var maxLen, cur uint32
	comCall(b, slotBufLock, uintptr(unsafe.Pointer(&ptr)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&cur)))
	copy(unsafe.Slice((*byte)(ptr), maxLen), au)
	comCall(b, slotBufUnlock)
	comCall(b, slotBufSetCurLn, uintptr(len(au)))
	release(b)
	r := comCall(p.dec, slotMFTProcessInput, 0, uintptr(unsafe.Pointer(s)), 0)
	release(s)
	if err := hr("ProcessInput(decoder)", r); err != nil {
		return false, err
	}
	for {
		var db mftOutputDataBuffer
		var status uint32
		r := comCall(p.dec, slotMFTProcessOutput, 0, 1, uintptr(unsafe.Pointer(&db)), uintptr(unsafe.Pointer(&status)))
		release(db.Events)
		switch uint32(r) {
		case mfETransformNeedMoreIn:
			return shown, nil
		case mfETransformStreamChange:
			if err := p.pickNV12(); err != nil {
				return shown, err
			}
			p.closeVP()
			continue
		}
		if err := hr("ProcessOutput(decoder)", r); err != nil {
			release(db.Sample)
			return shown, err
		}
		p.Decoded++
		release(p.last)
		p.last = db.Sample
		if err := p.show(); err != nil {
			return shown, err
		}
		shown = true
	}
}

// Resize follows the window's new client size and shows the last frame again.
func (p *Presenter) Resize() error {
	w, h := clientSize(p.hwnd)
	if w == p.bufW && h == p.bufH || w == 0 || h == 0 {
		return nil
	}
	p.closeVP()
	release(p.outView)
	p.outView = nil
	p.flushContext()
	if err := hr("ResizeBuffers", comCall(p.swap, slotSwapResizeBuffers, 0, uintptr(w), uintptr(h), 0, 0)); err != nil {
		return err
	}
	p.bufW, p.bufH = w, h
	if p.last != nil {
		return p.show()
	}
	return nil
}

func (p *Presenter) flushContext() {
	const slotCtxClearState, slotCtxFlush = 110, 111
	comCall(p.ctx, slotCtxClearState)
	comCall(p.ctx, slotCtxFlush)
}

// Redraw shows the last frame again.
func (p *Presenter) Redraw() error {
	if p.last == nil {
		return nil
	}
	return p.show()
}

// Dest is where the picture is in the window's client area (letterboxed).
func (p *Presenter) Dest() (x, y, w, h int) {
	d := p.dest
	return int(d.Left), int(d.Top), int(d.Right - d.Left), int(d.Bottom - d.Top)
}

func (p *Presenter) closeVP() {
	release(p.vp)
	release(p.vpEnum)
	p.vp, p.vpEnum = nil, nil
}

func (p *Presenter) ensureVP() error {
	if p.vp != nil {
		return nil
	}
	if p.texW == 0 {
		p.texW, p.texH = p.frameW, p.frameH
	}
	cd := vpContentDesc{InputWidth: uint32(p.texW), InputHeight: uint32(p.texH), OutputWidth: uint32(p.bufW), OutputHeight: uint32(p.bufH),
		InRateNum: 60, InRateDen: 1, OutRateNum: 60, OutRateDen: 1}
	if err := hr("CreateVideoProcessorEnumerator", comCall(p.vdev, slotVDevCreateVPEnum, uintptr(unsafe.Pointer(&cd)), uintptr(unsafe.Pointer(&p.vpEnum)))); err != nil {
		return err
	}
	if err := hr("CreateVideoProcessor", comCall(p.vdev, slotVDevCreateVP, uintptr(unsafe.Pointer(p.vpEnum)), 0, uintptr(unsafe.Pointer(&p.vp)))); err != nil {
		return err
	}
	// Input: BT.709, studio range (what the encoder writes); output: full-range RGB.
	in, out := uint32(1<<2|1<<4), uint32(0)
	comCall(p.vctx, slotVCtxSetStreamColorSpace, uintptr(unsafe.Pointer(p.vp)), 0, uintptr(unsafe.Pointer(&in)))
	comCall(p.vctx, slotVCtxSetOutputColorSpace, uintptr(unsafe.Pointer(p.vp)), uintptr(unsafe.Pointer(&out)))
	comCall(p.vctx, slotVCtxSetStreamFrameFmt, uintptr(unsafe.Pointer(p.vp)), 0, 0) // progressive
	comCall(p.vctx, slotVCtxSetStreamAutoProc, uintptr(unsafe.Pointer(p.vp)), 0, 0) // no "enhancements"
	black := [4]float32{0, 0, 0, 1}
	comCall(p.vctx, slotVCtxSetOutputBgColor, uintptr(unsafe.Pointer(p.vp)), 0, uintptr(unsafe.Pointer(&black)))
	src := rect{0, 0, int32(p.frameW), int32(p.frameH)}
	comCall(p.vctx, slotVCtxSetStreamSourceRect, uintptr(unsafe.Pointer(p.vp)), 0, 1, uintptr(unsafe.Pointer(&src)))
	// Letterbox: fit the picture, keep its aspect.
	bw, bh, fw, fh := p.bufW, p.bufH, p.frameW, p.frameH
	w, h := bw, fh*bw/fw
	if h > bh {
		w, h = fw*bh/fh, bh
	}
	p.dest = rect{int32((bw - w) / 2), int32((bh - h) / 2), int32((bw-w)/2 + w), int32((bh-h)/2 + h)}
	comCall(p.vctx, slotVCtxSetStreamDestRect, uintptr(unsafe.Pointer(p.vp)), 0, 1, uintptr(unsafe.Pointer(&p.dest)))
	full := rect{0, 0, int32(bw), int32(bh)}
	comCall(p.vctx, slotVCtxSetOutputTargetRect, uintptr(unsafe.Pointer(p.vp)), 1, uintptr(unsafe.Pointer(&full)))
	return nil
}

func (p *Presenter) show() error {
	if err := p.ensureVP(); err != nil {
		return err
	}
	// The decoded surface: a slice of the decoder's texture array.
	var buf, dbuf, tex *comObj
	if err := hr("GetBufferByIndex", comCall(p.last, slotSampleGetBufferByIndex, 0, uintptr(unsafe.Pointer(&buf)))); err != nil {
		return err
	}
	defer release(buf)
	if err := hr("QueryInterface(IMFDXGIBuffer)", comCall(buf, 0, uintptr(unsafe.Pointer(&iidIMFDXGIBuffer)), uintptr(unsafe.Pointer(&dbuf)))); err != nil {
		return fmt.Errorf("the decoder did not decode on the GPU: %w", err)
	}
	defer release(dbuf)
	comCall(dbuf, slotDXGIBufGetResource, uintptr(unsafe.Pointer(&iidID3D11Texture2D)), uintptr(unsafe.Pointer(&tex)))
	defer release(tex)
	var slice uint32
	comCall(dbuf, slotDXGIBufGetSubresource, uintptr(unsafe.Pointer(&slice)))
	ivd := vpInputViewDesc{ViewDimension: 1, ArraySlice: slice}
	var inView *comObj
	if err := hr("CreateVideoProcessorInputView", comCall(p.vdev, slotVDevCreateInputView, uintptr(unsafe.Pointer(tex)), uintptr(unsafe.Pointer(p.vpEnum)),
		uintptr(unsafe.Pointer(&ivd)), uintptr(unsafe.Pointer(&inView)))); err != nil {
		return err
	}
	defer release(inView)
	if p.outView == nil {
		var back *comObj
		if err := hr("GetBuffer", comCall(p.swap, slotSwapGetBuffer, 0, uintptr(unsafe.Pointer(&iidID3D11Texture2D)), uintptr(unsafe.Pointer(&back)))); err != nil {
			return err
		}
		ovd := vpOutputViewDesc{ViewDimension: 1}
		r := comCall(p.vdev, slotVDevCreateOutputView, uintptr(unsafe.Pointer(back)), uintptr(unsafe.Pointer(p.vpEnum)),
			uintptr(unsafe.Pointer(&ovd)), uintptr(unsafe.Pointer(&p.outView)))
		release(back)
		if err := hr("CreateVideoProcessorOutputView", r); err != nil {
			return err
		}
	}
	st := vpStream{Enable: 1, InputSurface: inView}
	if err := hr("VideoProcessorBlt", comCall(p.vctx, slotVCtxBlt, uintptr(unsafe.Pointer(p.vp)), uintptr(unsafe.Pointer(p.outView)), 0, 1, uintptr(unsafe.Pointer(&st)))); err != nil {
		return err
	}
	if p.Snapshot != nil {
		if err := p.snapshot(); err != nil {
			return err
		}
	}
	// Flip-discard: the output view must be made again after each Present.
	release(p.outView)
	p.outView = nil
	return hr("Present", comCall(p.swap, slotSwapPresent, 0, 0))
}

// snapshot reads the back buffer back (what is about to be presented) and
// hands it to Snapshot once (BGRA, stride w*4): a check without looking.
func (p *Presenter) snapshot() error {
	var back, staging *comObj
	if err := hr("GetBuffer", comCall(p.swap, slotSwapGetBuffer, 0, uintptr(unsafe.Pointer(&iidID3D11Texture2D)), uintptr(unsafe.Pointer(&back)))); err != nil {
		return err
	}
	defer release(back)
	td := texture2DDesc{Width: uint32(p.bufW), Height: uint32(p.bufH), MipLevels: 1, ArraySize: 1, Format: dxgiFormatB8G8R8A8,
		SampleCount: 1, Usage: d3d11UsageStaging, CPUAccessFlags: d3d11CPUAccessRead}
	if err := hr("CreateTexture2D", comCall(p.device, slotDeviceCreateTexture2D, uintptr(unsafe.Pointer(&td)), 0, uintptr(unsafe.Pointer(&staging)))); err != nil {
		return err
	}
	defer release(staging)
	comCall(p.ctx, slotCtxCopyResource, uintptr(unsafe.Pointer(staging)), uintptr(unsafe.Pointer(back)))
	var m mappedSubresource
	if err := hr("Map", comCall(p.ctx, slotCtxMap, uintptr(unsafe.Pointer(staging)), 0, d3d11MapRead, 0, uintptr(unsafe.Pointer(&m)))); err != nil {
		return err
	}
	row := p.bufW * 4
	src := unsafe.Slice((*byte)(m.Data), int(m.RowPitch)*(p.bufH-1)+row)
	out := make([]byte, row*p.bufH)
	for y := 0; y < p.bufH; y++ {
		copy(out[y*row:(y+1)*row], src[y*int(m.RowPitch):])
	}
	comCall(p.ctx, slotCtxUnmap, uintptr(unsafe.Pointer(staging)), 0)
	f := p.Snapshot
	p.Snapshot = nil
	f(out, p.bufW, p.bufH)
	return nil
}

// Close releases everything.
func (p *Presenter) Close() {
	release(p.last)
	release(p.outView)
	p.closeVP()
	release(p.dec)
	release(p.mgr)
	release(p.swap)
	release(p.vctx)
	release(p.vdev)
	release(p.ctx)
	release(p.device)
	*p = Presenter{}
}
