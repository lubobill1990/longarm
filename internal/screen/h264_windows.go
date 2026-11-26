//go:build windows

package screen

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// H.264 through Media Foundation encoder MFTs: the GPU's (NVENC, Quick
// Sync, AMF: asynchronous "V2" MFTs driven by events) or Microsoft's
// software encoder (synchronous). Input is NV12 in system memory; output an
// Annex B byte stream (what WebCodecs decodes without a description).

var (
	modMFPlat = windows.NewLazySystemDLL("mfplat.dll")
	modOle32  = windows.NewLazySystemDLL("ole32.dll")

	procMFStartup            = modMFPlat.NewProc("MFStartup")
	procMFTEnumEx            = modMFPlat.NewProc("MFTEnumEx")
	procMFCreateMediaType    = modMFPlat.NewProc("MFCreateMediaType")
	procMFCreateSample       = modMFPlat.NewProc("MFCreateSample")
	procMFCreateMemoryBuffer = modMFPlat.NewProc("MFCreateMemoryBuffer")
	procCoInitializeEx       = modOle32.NewProc("CoInitializeEx")
	procCoTaskMemFree        = modOle32.NewProc("CoTaskMemFree")
)

func guid(d1 uint32, d2, d3 uint16, d4 ...byte) windows.GUID {
	g := windows.GUID{Data1: d1, Data2: d2, Data3: d3}
	copy(g.Data4[:], d4)
	return g
}

// fourCC is a Media Foundation video subtype: the FOURCC in the base GUID.
func fourCC(s string) windows.GUID {
	return guid(uint32(s[0])|uint32(s[1])<<8|uint32(s[2])<<16|uint32(s[3])<<24, 0, 0x10, 0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71)
}

var (
	mfMediaTypeVideo     = guid(0x73646976, 0, 0x10, 0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71)
	mfVideoFormatNV12    = fourCC("NV12")
	mfVideoFormatH264    = fourCC("H264")
	mftCategoryVideoEnc  = guid(0xf79eac7d, 0xe545, 0x4387, 0xbd, 0xee, 0xd6, 0x47, 0xd7, 0xbd, 0xe4, 0x2a)
	mfMTMajorType        = guid(0x48eba18e, 0xf8c9, 0x4687, 0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f)
	mfMTSubtype          = guid(0xf7e34c9a, 0x42e8, 0x4714, 0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5)
	mfMTFrameSize        = guid(0x1652c33d, 0xd6b2, 0x4012, 0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d)
	mfMTFrameRate        = guid(0xc459a2e8, 0x3d2c, 0x4e44, 0xb1, 0x32, 0xfe, 0xe5, 0x15, 0x6c, 0x7b, 0xb0)
	mfMTPixelAspect      = guid(0xc6376a1e, 0x8d0a, 0x4027, 0xbe, 0x45, 0x6d, 0x9a, 0x0a, 0xd3, 0x9b, 0xb6)
	mfMTInterlaceMode    = guid(0xe2724bb8, 0xe676, 0x4806, 0xb4, 0xb2, 0xa8, 0xd6, 0xef, 0xb4, 0x4c, 0xcd)
	mfMTAvgBitrate       = guid(0x20332624, 0xfb0d, 0x4d9e, 0xbd, 0x0d, 0xcb, 0xf6, 0x78, 0x6c, 0x10, 0x2e)
	mfMTMpeg2Profile     = guid(0xad76a80b, 0x2d5c, 0x4e0b, 0xb3, 0x75, 0x64, 0xe5, 0x20, 0x13, 0x70, 0x36)
	mfLowLatency         = guid(0x9c27891a, 0xed7a, 0x40e1, 0x88, 0xe8, 0xb2, 0x27, 0x27, 0xa0, 0x24, 0xee) // = CODECAPI_AVLowLatencyMode
	mfTransformAsyncUnlk = guid(0xe5666d6b, 0x3422, 0x4eb6, 0xa4, 0x21, 0xda, 0x7d, 0xb1, 0xf8, 0xe2, 0x07)
	codecRateControlMode = guid(0x1c0608e9, 0x370c, 0x4710, 0x8a, 0x58, 0xcb, 0x61, 0x81, 0xc4, 0x24, 0x23)
	codecMeanBitRate     = guid(0xf7222374, 0x2144, 0x4815, 0xb5, 0x50, 0xa3, 0x7f, 0x8e, 0x12, 0xee, 0x52)
	codecGOPSize         = guid(0x95f31b26, 0x95a4, 0x41aa, 0x93, 0x03, 0x24, 0x6a, 0x7f, 0xc6, 0xee, 0xf1)
	codecForceKeyFrame   = guid(0x398c1b98, 0x8353, 0x475a, 0x9e, 0xf2, 0x8f, 0x26, 0x5d, 0x26, 0x03, 0x45)

	iidIMFTransform           = guid(0xbf94c121, 0x5b05, 0x4e6f, 0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d)
	iidICodecAPI              = guid(0x901db4c7, 0x31ce, 0x41a2, 0x85, 0xdc, 0x8f, 0xa0, 0xbf, 0x41, 0xb8, 0xda)
	iidIMFMediaEventGenerator = guid(0x2cd0bd52, 0xbcd5, 0x4b89, 0xb6, 0x2c, 0xea, 0xdc, 0x0c, 0x03, 0x1e, 0x7d)
)

const (
	mfVersion = 0x00020070

	mftEnumSyncMFT       = 0x01
	mftEnumHardware      = 0x04
	mftEnumSortAndFilter = 0x40

	// IMFAttributes (IMFMediaType, IMFActivate, IMFSample share these).
	slotAttrGetUINT32 = 7
	slotAttrSetUINT32 = 21
	slotAttrSetUINT64 = 22
	slotAttrSetGUID   = 24
	slotActivateObj   = 33

	slotMFTGetOutputStreamInfo = 7
	slotMFTGetAttributes       = 8
	slotMFTGetOutputAvailType  = 14
	slotMFTSetInputType        = 15
	slotMFTSetOutputType       = 16
	slotMFTProcessMessage      = 23
	slotMFTProcessInput        = 24
	slotMFTProcessOutput       = 25

	slotSampleSetTime       = 36
	slotSampleSetDuration   = 38
	slotSampleContiguousBuf = 41
	slotSampleAddBuffer     = 42

	slotBufLock     = 3
	slotBufUnlock   = 4
	slotBufCurLen   = 5
	slotBufSetCurLn = 6

	slotEvGenGetEvent = 3
	slotEventGetType  = 33
	slotEventStatus   = 35

	slotCodecSetValue = 9

	meTransformNeedInput  = 601
	meTransformHaveOutput = 602

	mftMsgBeginStreaming = 0x10000000
	mftMsgStartOfStream  = 0x10000003

	mftOutputProvidesSamples = 0x100
	mfETransformNeedMoreIn   = 0xC00D6D72
	mfETransformStreamChange = 0xC00D6D61
	mfENotAccepting          = 0xC00D36B5

	vtUI4  = 19
	vtBool = 11

	profileHigh = 100
	rcCBR       = 0
)

type mftRegisterTypeInfo struct{ Major, Sub windows.GUID }

type mftOutputDataBuffer struct {
	StreamID uint32
	_        uint32
	Sample   *comObj
	Status   uint32
	_        uint32
	Events   *comObj
}

type mftOutputStreamInfo struct{ Flags, Size, Alignment uint32 }

// variant is a VARIANT holding a VT_UI4 or VT_BOOL (24 bytes on 64-bit).
type variant struct {
	VT  uint16
	_   [3]uint16
	Val uint64
	_   uint64
}

var mfOnce sync.Once
var mfErr error

func mfStartup() error {
	mfOnce.Do(func() {
		if err := modMFPlat.Load(); err != nil {
			mfErr = fmt.Errorf("Media Foundation is missing (Windows N without the Media Feature Pack?): %w", err)
			return
		}
		r, _, _ := procMFStartup.Call(mfVersion, 0)
		mfErr = hr("MFStartup", r)
	})
	return mfErr
}

// Encoder is an H.264 encoder. Use it from one goroutine (it locks the
// thread: COM's apartment and the async MFT's event loop).
type Encoder struct {
	Name  string // "hardware" or "software"
	opts  EncoderOptions
	mft   *comObj
	codec *comObj // ICodecAPI, or nil
	evgen *comObj // async MFTs
	start time.Time

	needInput       int // async: NeedInput events not yet answered
	providesSamples bool
	outSize         uint32
}

// NewEncoder opens the GPU's encoder, else the software one.
func NewEncoder(o EncoderOptions) (*Encoder, error) {
	runtime.LockOSThread()
	procCoInitializeEx.Call(0, 0) // COINIT_MULTITHREADED
	if err := mfStartup(); err != nil {
		return nil, err
	}
	if o.Width%2 != 0 || o.Height%2 != 0 {
		return nil, fmt.Errorf("H.264: %dx%d: width and height must be even", o.Width, o.Height)
	}
	var errs []error
	if !o.Software {
		e, err := openEncoder(o, true)
		if err == nil {
			return e, nil
		}
		errs = append(errs, fmt.Errorf("hardware: %w", err))
	}
	e, err := openEncoder(o, false)
	if err == nil {
		return e, nil
	}
	errs = append(errs, fmt.Errorf("software: %w", err))
	return nil, errors.Join(errs...)
}

func openEncoder(o EncoderOptions, hardware bool) (*Encoder, error) {
	flags := uintptr(mftEnumSyncMFT | mftEnumSortAndFilter)
	if hardware {
		flags = mftEnumHardware | mftEnumSortAndFilter
	}
	in := mftRegisterTypeInfo{mfMediaTypeVideo, mfVideoFormatNV12}
	out := mftRegisterTypeInfo{mfMediaTypeVideo, mfVideoFormatH264}
	var acts **comObj
	var n uint32
	r, _, _ := procMFTEnumEx.Call(uintptr(unsafe.Pointer(&mftCategoryVideoEnc)), flags,
		uintptr(unsafe.Pointer(&in)), uintptr(unsafe.Pointer(&out)), uintptr(unsafe.Pointer(&acts)), uintptr(unsafe.Pointer(&n)))
	if err := hr("MFTEnumEx", r); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("no H.264 encoder")
	}
	list := unsafe.Slice(acts, n)
	defer func() {
		for _, a := range list {
			release(a)
		}
		procCoTaskMemFree.Call(uintptr(unsafe.Pointer(acts)))
	}()
	var errs []error
	for _, a := range list {
		e := &Encoder{opts: o, Name: "software"}
		if hardware {
			e.Name = "hardware"
		}
		if err := hr("ActivateObject", comCall(a, slotActivateObj, uintptr(unsafe.Pointer(&iidIMFTransform)), uintptr(unsafe.Pointer(&e.mft)))); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := e.setup(hardware); err != nil {
			errs = append(errs, err)
			e.Close()
			continue
		}
		return e, nil
	}
	return nil, errors.Join(errs...)
}

func (e *Encoder) setup(async bool) error {
	o := e.opts
	if async {
		var attrs *comObj
		if err := hr("GetAttributes", comCall(e.mft, slotMFTGetAttributes, uintptr(unsafe.Pointer(&attrs)))); err != nil {
			return err
		}
		comCall(attrs, slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfTransformAsyncUnlk)), 1)
		comCall(attrs, slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfLowLatency)), 1)
		release(attrs)
		if err := hr("QueryInterface(IMFMediaEventGenerator)", comCall(e.mft, 0, uintptr(unsafe.Pointer(&iidIMFMediaEventGenerator)), uintptr(unsafe.Pointer(&e.evgen)))); err != nil {
			return err
		}
	}
	if comCall(e.mft, 0, uintptr(unsafe.Pointer(&iidICodecAPI)), uintptr(unsafe.Pointer(&e.codec))) != 0 {
		e.codec = nil
	}
	if e.codec != nil {
		// Before the types: some encoders only take these then.
		e.setCodec(&mfLowLatency, vtBool, 0xFFFF)
		e.setCodec(&codecRateControlMode, vtUI4, rcCBR)
		e.setCodec(&codecMeanBitRate, vtUI4, uint64(o.Bitrate))
		e.setCodec(&codecGOPSize, vtUI4, uint64(o.FPS*10)) // a keyframe every ~10 s of motion; more on request
	}
	size := uint64(o.Width)<<32 | uint64(o.Height)
	rate := uint64(o.FPS)<<32 | 1
	outType, err := mediaType(func(t *comObj) {
		comCall(t, slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTMajorType)), uintptr(unsafe.Pointer(&mfMediaTypeVideo)))
		comCall(t, slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&mfVideoFormatH264)))
		comCall(t, slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfMTAvgBitrate)), uintptr(o.Bitrate))
		comCall(t, slotAttrSetUINT64, uintptr(unsafe.Pointer(&mfMTFrameSize)), uintptr(size))
		comCall(t, slotAttrSetUINT64, uintptr(unsafe.Pointer(&mfMTFrameRate)), uintptr(rate))
		comCall(t, slotAttrSetUINT64, uintptr(unsafe.Pointer(&mfMTPixelAspect)), uintptr(uint64(1)<<32|1))
		comCall(t, slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfMTInterlaceMode)), 2) // progressive
		comCall(t, slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfMTMpeg2Profile)), profileHigh)
	})
	if err != nil {
		return err
	}
	defer release(outType)
	if err := hr("SetOutputType", comCall(e.mft, slotMFTSetOutputType, 0, uintptr(unsafe.Pointer(outType)), 0)); err != nil {
		return err
	}
	inType, err := mediaType(func(t *comObj) {
		comCall(t, slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTMajorType)), uintptr(unsafe.Pointer(&mfMediaTypeVideo)))
		comCall(t, slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&mfVideoFormatNV12)))
		comCall(t, slotAttrSetUINT64, uintptr(unsafe.Pointer(&mfMTFrameSize)), uintptr(size))
		comCall(t, slotAttrSetUINT64, uintptr(unsafe.Pointer(&mfMTFrameRate)), uintptr(rate))
		comCall(t, slotAttrSetUINT64, uintptr(unsafe.Pointer(&mfMTPixelAspect)), uintptr(uint64(1)<<32|1))
		comCall(t, slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfMTInterlaceMode)), 2)
	})
	if err != nil {
		return err
	}
	defer release(inType)
	if err := hr("SetInputType", comCall(e.mft, slotMFTSetInputType, 0, uintptr(unsafe.Pointer(inType)), 0)); err != nil {
		return err
	}
	var si mftOutputStreamInfo
	comCall(e.mft, slotMFTGetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&si)))
	e.providesSamples = si.Flags&mftOutputProvidesSamples != 0
	e.outSize = max(si.Size, uint32(o.Width*o.Height))
	comCall(e.mft, slotMFTProcessMessage, mftMsgBeginStreaming, 0)
	comCall(e.mft, slotMFTProcessMessage, mftMsgStartOfStream, 0)
	e.start = time.Now()
	return nil
}

func (e *Encoder) setCodec(g *windows.GUID, vt uint16, v uint64) {
	val := variant{VT: vt, Val: v}
	comCall(e.codec, slotCodecSetValue, uintptr(unsafe.Pointer(g)), uintptr(unsafe.Pointer(&val)))
}

func mediaType(set func(*comObj)) (*comObj, error) {
	var t *comObj
	r, _, _ := procMFCreateMediaType.Call(uintptr(unsafe.Pointer(&t)))
	if err := hr("MFCreateMediaType", r); err != nil {
		return nil, err
	}
	set(t)
	return t, nil
}

// Close releases the encoder.
func (e *Encoder) Close() {
	release(e.codec)
	release(e.evgen)
	release(e.mft)
	e.codec, e.evgen, e.mft = nil, nil, nil
	runtime.UnlockOSThread()
}

// ForceKeyframe makes the next frame an IDR frame (a viewer joined or lost data).
func (e *Encoder) ForceKeyframe() {
	if e.codec != nil {
		e.setCodec(&codecForceKeyFrame, vtUI4, 1)
	}
}

// Encode encodes one NV12 frame (Width*Height*3/2 bytes) and returns the
// Annex B data that came out (it can be empty while the encoder fills its
// pipeline; low-latency mode keeps that to none or one frame).
func (e *Encoder) Encode(nv12 []byte) ([]byte, error) {
	sample, err := e.inputSample(nv12)
	if err != nil {
		return nil, err
	}
	defer release(sample)
	if e.evgen == nil {
		if err := hr("ProcessInput", comCall(e.mft, slotMFTProcessInput, 0, uintptr(unsafe.Pointer(sample)), 0)); err != nil {
			return nil, err
		}
		return e.drain()
	}
	// Async: wait for NeedInput, feed, then collect what HaveOutput says
	// is ready until the encoder asks for more.
	var out []byte
	fed := false
	for {
		if fed && e.needInput > 0 {
			return out, nil
		}
		if !fed && e.needInput > 0 {
			e.needInput--
			if err := hr("ProcessInput", comCall(e.mft, slotMFTProcessInput, 0, uintptr(unsafe.Pointer(sample)), 0)); err != nil {
				return nil, err
			}
			fed = true
			continue
		}
		var ev *comObj
		if err := hr("GetEvent", comCall(e.evgen, slotEvGenGetEvent, 0, uintptr(unsafe.Pointer(&ev)))); err != nil {
			return nil, err
		}
		var typ uint32
		var status uint32
		comCall(ev, slotEventGetType, uintptr(unsafe.Pointer(&typ)))
		comCall(ev, slotEventStatus, uintptr(unsafe.Pointer(&status)))
		release(ev)
		if int32(status) < 0 {
			return nil, fmt.Errorf("encoder event %d: HRESULT 0x%08X", typ, status)
		}
		switch typ {
		case meTransformNeedInput:
			e.needInput++
		case meTransformHaveOutput:
			b, err := e.output()
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
			if fed {
				return out, nil
			}
		}
	}
}

// drain collects a synchronous MFT's output until it needs more input.
func (e *Encoder) drain() ([]byte, error) {
	var out []byte
	for {
		b, err := e.output()
		if err == errNeedMoreInput {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
}

var errNeedMoreInput = errors.New("need more input")

func (e *Encoder) output() ([]byte, error) {
	var db mftOutputDataBuffer
	if !e.providesSamples {
		s, err := newSample(int(e.outSize))
		if err != nil {
			return nil, err
		}
		db.Sample = s
	}
	var status uint32
	r := comCall(e.mft, slotMFTProcessOutput, 0, 1, uintptr(unsafe.Pointer(&db)), uintptr(unsafe.Pointer(&status)))
	defer release(db.Sample)
	defer release(db.Events)
	switch uint32(r) {
	case mfETransformNeedMoreIn:
		return nil, errNeedMoreInput
	case mfETransformStreamChange:
		// The encoder settled its output type: take it again and go on.
		var t *comObj
		if comCall(e.mft, slotMFTGetOutputAvailType, 0, 0, uintptr(unsafe.Pointer(&t))) == 0 {
			comCall(e.mft, slotMFTSetOutputType, 0, uintptr(unsafe.Pointer(t)), 0)
			release(t)
		}
		return nil, nil
	}
	if err := hr("ProcessOutput", r); err != nil {
		return nil, err
	}
	return sampleBytes(db.Sample)
}

func newSample(size int) (*comObj, error) {
	var s, b *comObj
	r, _, _ := procMFCreateSample.Call(uintptr(unsafe.Pointer(&s)))
	if err := hr("MFCreateSample", r); err != nil {
		return nil, err
	}
	r, _, _ = procMFCreateMemoryBuffer.Call(uintptr(size), uintptr(unsafe.Pointer(&b)))
	if err := hr("MFCreateMemoryBuffer", r); err != nil {
		release(s)
		return nil, err
	}
	comCall(s, slotSampleAddBuffer, uintptr(unsafe.Pointer(b)))
	release(b)
	return s, nil
}

func (e *Encoder) inputSample(nv12 []byte) (*comObj, error) {
	s, err := newSample(len(nv12))
	if err != nil {
		return nil, err
	}
	var b *comObj
	comCall(s, slotSampleContiguousBuf, uintptr(unsafe.Pointer(&b)))
	var p unsafe.Pointer
	var maxLen, cur uint32
	if err := hr("Lock", comCall(b, slotBufLock, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&cur)))); err != nil {
		release(b)
		release(s)
		return nil, err
	}
	copy(unsafe.Slice((*byte)(p), maxLen), nv12)
	comCall(b, slotBufUnlock)
	comCall(b, slotBufSetCurLn, uintptr(len(nv12)))
	release(b)
	t := time.Since(e.start) / 100 // 100 ns units
	comCall(s, slotSampleSetTime, uintptr(t))
	comCall(s, slotSampleSetDuration, uintptr(time.Second/100/time.Duration(e.opts.FPS)))
	return s, nil
}

func sampleBytes(s *comObj) ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	var b *comObj
	if err := hr("ConvertToContiguousBuffer", comCall(s, slotSampleContiguousBuf, uintptr(unsafe.Pointer(&b)))); err != nil {
		return nil, err
	}
	defer release(b)
	var p unsafe.Pointer
	var maxLen, cur uint32
	if err := hr("Lock", comCall(b, slotBufLock, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&cur)))); err != nil {
		return nil, err
	}
	out := append([]byte(nil), unsafe.Slice((*byte)(p), cur)...)
	comCall(b, slotBufUnlock)
	return out, nil
}
