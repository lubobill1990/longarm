//go:build windows

package screen

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// h264Enc is a VideoEncoder over Encoder (which takes NV12).
type h264Enc struct {
	e    *Encoder
	nv12 []byte
	w, h int
}

func newH264Encoder(o EncoderOptions) (VideoEncoder, error) {
	e, err := NewEncoder(o)
	if err != nil {
		return nil, err
	}
	return &h264Enc{e: e, nv12: make([]byte, o.Width*o.Height*3/2), w: o.Width, h: o.Height}, nil
}

func (h *h264Enc) Name() string   { return h.e.Name }
func (h *h264Enc) ForceKeyframe() { h.e.ForceKeyframe() }
func (h *h264Enc) Close()         { h.e.Close() }

func (h *h264Enc) Encode(bgra []byte) ([]byte, error) {
	BGRAToNV12(h.nv12, bgra, h.w, h.h)
	return h.e.Encode(h.nv12)
}

var mfMTDefaultStride = guid(0x644b4e48, 0x1e02, 0x4516, 0xb0, 0xeb, 0xc0, 0x1c, 0xa9, 0xd4, 0x9a, 0xc6)

// openH264Decoder activates Media Foundation's H.264 decoder for w×h
// pictures, low latency; with mgr (a DXGI device manager) it decodes on
// the GPU.
func openH264Decoder(w, h int, mgr *comObj) (*comObj, error) {
	in := mftRegisterTypeInfo{mfMediaTypeVideo, mfVideoFormatH264}
	out := mftRegisterTypeInfo{mfMediaTypeVideo, mfVideoFormatNV12}
	var acts **comObj
	var n uint32
	r, _, _ := procMFTEnumEx.Call(uintptr(unsafe.Pointer(&mftCategoryVideoDecoder)), mftEnumSyncMFT|mftEnumSortAndFilter,
		uintptr(unsafe.Pointer(&in)), uintptr(unsafe.Pointer(&out)), uintptr(unsafe.Pointer(&acts)), uintptr(unsafe.Pointer(&n)))
	if err := hr("MFTEnumEx(decoder)", r); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("no H.264 decoder")
	}
	list := unsafe.Slice(acts, n)
	defer func() {
		for _, a := range list {
			release(a)
		}
		procCoTaskMemFree.Call(uintptr(unsafe.Pointer(acts)))
	}()
	// The first decoder that activates (a CI runner's once answered S_OK
	// with no object: the next one, or an error, not a crash).
	var dec *comObj
	err := errors.New("ActivateObject(decoder): no object")
	for _, a := range list {
		if a == nil {
			continue
		}
		if err = hr("ActivateObject(decoder)", comCall(a, slotActivateObj, uintptr(unsafe.Pointer(&iidIMFTransform)), uintptr(unsafe.Pointer(&dec)))); err == nil && dec != nil {
			break
		}
		dec = nil
	}
	if dec == nil {
		return nil, err
	}
	var attrs *comObj
	if comCall(dec, slotMFTGetAttributes, uintptr(unsafe.Pointer(&attrs))) == 0 && attrs != nil {
		comCall(attrs, slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfLowLatency)), 1) // a frame out per frame in
		release(attrs)
	}
	if mgr != nil {
		if err := hr("SET_D3D_MANAGER", comCall(dec, slotMFTProcessMessage, mftMsgSetD3DManager, uintptr(unsafe.Pointer(mgr)))); err != nil {
			release(dec)
			return nil, fmt.Errorf("the decoder cannot decode on the GPU: %w", err)
		}
	}
	t, err := mediaType(func(t *comObj) {
		comCall(t, slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTMajorType)), uintptr(unsafe.Pointer(&mfMediaTypeVideo)))
		comCall(t, slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&mfVideoFormatH264)))
		comCall(t, slotAttrSetUINT64, uintptr(unsafe.Pointer(&mfMTFrameSize)), uintptr(uint64(w)<<32|uint64(h)))
	})
	if err != nil {
		release(dec)
		return nil, err
	}
	defer release(t)
	if err := hr("SetInputType(decoder)", comCall(dec, slotMFTSetInputType, 0, uintptr(unsafe.Pointer(t)), 0)); err != nil {
		release(dec)
		return nil, err
	}
	if _, _, _, err := pickNV12(dec); err != nil {
		release(dec)
		return nil, err
	}
	comCall(dec, slotMFTProcessMessage, mftMsgBeginStreaming, 0)
	comCall(dec, slotMFTProcessMessage, mftMsgStartOfStream, 0)
	return dec, nil
}

// pickNV12 sets the decoder's NV12 output type and reads its surface size
// (padded, e.g. 1088 for 1080) and row stride (0: unknown).
func pickNV12(dec *comObj) (texW, texH, stride int, err error) {
	for i := 0; ; i++ {
		var t *comObj
		if r := comCall(dec, slotMFTGetOutputAvailType, 0, uintptr(i), uintptr(unsafe.Pointer(&t))); int32(r) < 0 {
			return 0, 0, 0, fmt.Errorf("the decoder offers no NV12 output: HRESULT 0x%08X", uint32(r))
		}
		var sub windows.GUID
		comCall(t, slotAttrGetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&sub)))
		if sub != mfVideoFormatNV12 {
			release(t)
			continue
		}
		r := comCall(dec, slotMFTSetOutputType, 0, uintptr(unsafe.Pointer(t)), 0)
		var size uint64
		var st uint32
		comCall(t, slotAttrGetUINT64, uintptr(unsafe.Pointer(&mfMTFrameSize)), uintptr(unsafe.Pointer(&size)))
		comCall(t, slotAttrGetUINT32, uintptr(unsafe.Pointer(&mfMTDefaultStride)), uintptr(unsafe.Pointer(&st)))
		release(t)
		return int(size >> 32), int(uint32(size)), int(int32(st)), hr("SetOutputType(decoder)", r)
	}
}

// feed gives the decoder one access unit.
func feed(dec *comObj, au []byte) error {
	s, err := newSample(len(au))
	if err != nil {
		return err
	}
	defer release(s)
	var b *comObj
	comCall(s, slotSampleContiguousBuf, uintptr(unsafe.Pointer(&b)))
	var ptr unsafe.Pointer
	var maxLen, cur uint32
	comCall(b, slotBufLock, uintptr(unsafe.Pointer(&ptr)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&cur)))
	copy(unsafe.Slice((*byte)(ptr), maxLen), au)
	comCall(b, slotBufUnlock)
	comCall(b, slotBufSetCurLn, uintptr(len(au)))
	release(b)
	return hr("ProcessInput(decoder)", comCall(dec, slotMFTProcessInput, 0, uintptr(unsafe.Pointer(s)), 0))
}

// swDecoder decodes into system memory (no GPU needed).
type swDecoder struct {
	dec              *comObj
	w, h             int
	texW, texH, strd int
	outSize          uint32
	bgra             []byte
}

func newH264Decoder(w, h int) (VideoDecoder, error) {
	procCoInitializeEx.Call(0, 0)
	if err := mfStartup(); err != nil {
		return nil, err
	}
	dec, err := openH264Decoder(w, h, nil)
	if err != nil {
		return nil, err
	}
	d := &swDecoder{dec: dec, w: w, h: h, bgra: make([]byte, w*h*4)}
	d.texW, d.texH, d.strd, _ = pickNV12(dec)
	d.sizeOut()
	return d, nil
}

func (d *swDecoder) sizeOut() {
	var si mftOutputStreamInfo
	comCall(d.dec, slotMFTGetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&si)))
	d.outSize = max(si.Size, uint32(d.texW*d.texH*3/2))
}

func (d *swDecoder) Close() {
	release(d.dec)
	d.dec = nil
}

func (d *swDecoder) Decode(au []byte) ([]byte, int, int, error) {
	if err := feed(d.dec, au); err != nil {
		return nil, 0, 0, err
	}
	var img []byte
	for {
		s, err := newSample(int(d.outSize))
		if err != nil {
			return nil, 0, 0, err
		}
		db := mftOutputDataBuffer{Sample: s}
		var status uint32
		r := comCall(d.dec, slotMFTProcessOutput, 0, 1, uintptr(unsafe.Pointer(&db)), uintptr(unsafe.Pointer(&status)))
		release(db.Events)
		switch uint32(r) {
		case mfETransformNeedMoreIn:
			release(s)
			return img, d.w, d.h, nil
		case mfETransformStreamChange:
			release(s)
			if d.texW, d.texH, d.strd, err = pickNV12(d.dec); err != nil {
				return nil, 0, 0, err
			}
			d.sizeOut()
			continue
		}
		if err := hr("ProcessOutput(decoder)", r); err != nil {
			release(s)
			return nil, 0, 0, err
		}
		nv12, err := sampleBytes(db.Sample)
		release(db.Sample)
		if err != nil {
			return nil, 0, 0, err
		}
		stride := d.strd
		if stride <= 0 {
			stride = d.texW
		}
		texH := d.texH
		if texH == 0 {
			texH = d.h
		}
		if len(nv12) >= stride*texH*3/2 {
			NV12ToBGRA(d.bgra, nv12, stride, texH, d.w, d.h)
			img = d.bgra
		}
	}
}
