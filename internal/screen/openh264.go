//go:build linux && !noopenh264

package screen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// H.264 through Cisco's OpenH264 (openh264_fetch.go says where it comes
// from), loaded with purego: its C API is ISVCEncoder/ISVCDecoder, a
// pointer to a table of function pointers, each taking the object first
// (codec_api.h). The structures below mirror codec_app_def.h and
// codec_def.h of v2.7.0; 2.6 added PSNR fields (the soname went from .7
// to .8), so SLayerBSInfo's size depends on the version loaded.
//
// Importing purego makes Linux binaries link libc dynamically (it brings
// its own cgo runtime): build with -tags noopenh264 for a static binary
// without H.264.

type openH264Lib struct {
	mu        sync.Mutex
	loaded    bool
	err       error
	failedAt  time.Time
	createEnc uintptr // WelsCreateSVCEncoder
	destroyE  uintptr
	createDec uintptr // WelsCreateDecoder
	destroyD  uintptr
	layerSize uintptr // sizeof(SLayerBSInfo)
	version   [4]uint32
}

var openH264 openH264Lib

// openH264Download bounds the download a first H.264 session may wait for.
const openH264Download = 30 * time.Second

// loadOpenH264 finds the library (downloading it on first use); a failure
// is remembered for a while, so sessions fall back to JPEG tiles at once.
func loadOpenH264() error {
	o := &openH264
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.loaded {
		return nil
	}
	if o.err != nil && time.Since(o.failedAt) < 10*time.Minute {
		return o.err
	}
	var tried []string
	try := func(name string) bool {
		if name == "" {
			return false
		}
		err := o.open(name)
		if err != nil {
			tried = append(tried, err.Error())
		}
		return err == nil
	}
	if try(os.Getenv("LONGARM_OPENH264")) || try("libopenh264.so.8") || try("libopenh264.so.7") || try(openH264Verified()) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), openH264Download)
	defer cancel()
	p, err := DownloadOpenH264(ctx)
	if err == nil && try(p) {
		return nil
	}
	if err != nil {
		tried = append(tried, err.Error())
	}
	o.err = fmt.Errorf("%w: no OpenH264 (%s)", ErrUnsupported, strings.Join(tried, "; "))
	o.failedAt = time.Now()
	return o.err
}

func (o *openH264Lib) open(name string) error {
	lib, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return err
	}
	sym := func(s string) uintptr {
		p, e := purego.Dlsym(lib, s)
		if e != nil && err == nil {
			err = fmt.Errorf("%s: %w", name, e)
		}
		return p
	}
	o.createEnc, o.destroyE = sym("WelsCreateSVCEncoder"), sym("WelsDestroySVCEncoder")
	o.createDec, o.destroyD = sym("WelsCreateDecoder"), sym("WelsDestroyDecoder")
	ver := sym("WelsGetCodecVersionEx")
	if err != nil {
		purego.Dlclose(lib)
		return err
	}
	purego.SyscallN(ver, uintptr(unsafe.Pointer(&o.version)))
	// The vtables are the same since 1.x; 2.6 grew SLayerBSInfo.
	switch v := o.version; {
	case v[0] != 2:
		purego.Dlclose(lib)
		return fmt.Errorf("%s: OpenH264 %d.%d (2.x needed)", name, v[0], v[1])
	case v[1] >= 6:
		o.layerSize = unsafe.Sizeof(layerBSInfo{})
	default:
		o.layerSize = unsafe.Offsetof(layerBSInfo{}.psnr)
	}
	o.loaded, o.err = true, nil
	return nil
}

// OpenH264Version says which OpenH264 H.264 uses ("" until loaded).
func OpenH264Version() string {
	o := &openH264
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.loaded {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", o.version[0], o.version[1], o.version[2])
}

// vcall calls slot i of a C++-style object's function table.
func vcall(obj unsafe.Pointer, i int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(i)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := purego.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

// ISVCEncoder's slots.
const (
	encInitialize      = 0
	encUninitialize    = 3
	encEncodeFrame     = 4
	encForceIntraFrame = 6
	encSetOption       = 7
)

// ISVCDecoder's slots.
const (
	decInitialize         = 0
	decUninitialize       = 1
	decDecodeFrameNoDelay = 3
	decSetOption          = 8
)

const (
	screenContentRealTime = 1  // EUsageType
	rcBitrateMode         = 1  // RC_MODES
	videoFormatI420       = 23 // EVideoFormatType
	videoFrameTypeSkip    = 4
	optDataFormat         = 0 // ENCODER_OPTION
	optRCFrameSkip        = 9
	optEncTraceLevel      = 25
	optDecTraceLevel      = 9 // DECODER_OPTION
)

// SEncParamBase
type encParamBase struct {
	usage, width, height, bitrate, rcMode int32
	maxFrameRate                          float32
}

// SSourcePicture
type sourcePicture struct {
	colorFormat int32
	stride      [4]int32
	data        [4]unsafe.Pointer
	width       int32
	height      int32
	timestamp   int64
	psnr        [3]bool
}

// SLayerBSInfo (2.6+; earlier ones end before psnr).
type layerBSInfo struct {
	temporalID, spatialID, qualityID uint8
	frameType                        int32
	layerType                        uint8
	subSeqID                         int32
	nalCount                         int32
	nalLength                        *int32
	bs                               *byte
	psnr                             [3]float32
}

// SFrameBSInfo, sized for the larger SLayerBSInfo.
type frameBSInfo struct {
	layerNum  int32
	_         int32
	layers    [128 * unsafe.Sizeof(layerBSInfo{})]byte
	frameType int32
	size      int32
	timestamp int64
}

// SDecodingParam
type decodingParam struct {
	fileName    unsafe.Pointer
	cpuLoad     uint32
	targetDQ    uint8
	ecActiveIdc int32
	parseOnly   bool
	propSize    uint32 // SVideoProperty
	bsType      int32
}

// SBufferInfo
type bufferInfo struct {
	status        int32
	inTimestamp   uint64
	outTimestamp  uint64
	width, height int32
	format        int32
	stride        [2]int32
	dst           [3]*byte
}

type openH264Encoder struct {
	enc   unsafe.Pointer
	w, h  int
	yuv   []byte // I420
	nv12  []byte
	start time.Time
	key   bool
	info  frameBSInfo
	out   []byte
}

func newOpenH264Encoder(o EncoderOptions) (VideoEncoder, error) {
	if o.Width <= 0 || o.Height <= 0 || o.Width%2 != 0 || o.Height%2 != 0 {
		return nil, fmt.Errorf("H.264 needs an even size, not %dx%d", o.Width, o.Height)
	}
	if err := loadOpenH264(); err != nil {
		return nil, err
	}
	e := &openH264Encoder{w: o.Width, h: o.Height, start: time.Now()}
	if r, _, _ := purego.SyscallN(openH264.createEnc, uintptr(unsafe.Pointer(&e.enc))); r != 0 || e.enc == nil {
		return nil, fmt.Errorf("WelsCreateSVCEncoder: %d", int32(r))
	}
	quiet := int32(0) // WELS_LOG_QUIET: it writes warnings to stderr otherwise
	vcall(e.enc, encSetOption, optEncTraceLevel, uintptr(unsafe.Pointer(&quiet)))
	fps := o.FPS
	if fps <= 0 {
		fps = 30
	}
	p := encParamBase{usage: screenContentRealTime, width: int32(o.Width), height: int32(o.Height),
		bitrate: int32(o.Bitrate), rcMode: rcBitrateMode, maxFrameRate: float32(min(fps, 60))}
	if r := vcall(e.enc, encInitialize, uintptr(unsafe.Pointer(&p))); int32(r) != 0 {
		purego.SyscallN(openH264.destroyE, uintptr(e.enc))
		return nil, fmt.Errorf("OpenH264 encoder: Initialize: %d", int32(r))
	}
	format := int32(videoFormatI420)
	vcall(e.enc, encSetOption, optDataFormat, uintptr(unsafe.Pointer(&format)))
	// Frames come only when the screen changes: skipping one would leave
	// the old picture up.
	skip := false
	vcall(e.enc, encSetOption, optRCFrameSkip, uintptr(unsafe.Pointer(&skip)))
	e.yuv = make([]byte, o.Width*o.Height*3/2)
	e.nv12 = make([]byte, o.Width*o.Height*3/2)
	return e, nil
}

func (e *openH264Encoder) Name() string   { return "software" }
func (e *openH264Encoder) ForceKeyframe() { e.key = true }

func (e *openH264Encoder) Encode(bgra []byte) ([]byte, error) {
	w, h := e.w, e.h
	if len(bgra) < w*h*4 {
		return nil, errors.New("encode: short frame")
	}
	BGRAToNV12(e.nv12, bgra, w, h)
	copy(e.yuv, e.nv12[:w*h])
	u, v := e.yuv[w*h:w*h*5/4], e.yuv[w*h*5/4:]
	uv := e.nv12[w*h:]
	for i := range u {
		u[i], v[i] = uv[2*i], uv[2*i+1]
	}
	if e.key {
		e.key = false
		vcall(e.enc, encForceIntraFrame, 1)
	}
	pic := sourcePicture{colorFormat: videoFormatI420, stride: [4]int32{int32(w), int32(w / 2), int32(w / 2)},
		width: int32(w), height: int32(h), timestamp: time.Since(e.start).Milliseconds()}
	pic.data[0], pic.data[1], pic.data[2] = unsafe.Pointer(&e.yuv[0]), unsafe.Pointer(&u[0]), unsafe.Pointer(&v[0])
	e.info = frameBSInfo{}
	r := vcall(e.enc, encEncodeFrame, uintptr(unsafe.Pointer(&pic)), uintptr(unsafe.Pointer(&e.info)))
	runtime.KeepAlive(e.yuv)
	if int32(r) != 0 {
		return nil, fmt.Errorf("OpenH264 EncodeFrame: %d", int32(r))
	}
	e.out = e.out[:0]
	if e.info.frameType == videoFrameTypeSkip {
		return e.out, nil
	}
	for i := 0; i < int(e.info.layerNum) && i < 128; i++ {
		l := (*layerBSInfo)(unsafe.Pointer(&e.info.layers[uintptr(i)*openH264.layerSize]))
		n := 0
		for _, s := range unsafe.Slice(l.nalLength, l.nalCount) {
			n += int(s)
		}
		e.out = append(e.out, unsafe.Slice(l.bs, n)...)
	}
	return e.out, nil
}

func (e *openH264Encoder) Close() {
	if e.enc != nil {
		vcall(e.enc, encUninitialize)
		purego.SyscallN(openH264.destroyE, uintptr(e.enc))
		e.enc = nil
	}
}

type openH264Decoder struct {
	dec unsafe.Pointer
	img []byte
}

func newOpenH264Decoder(w, h int) (VideoDecoder, error) {
	if err := loadOpenH264(); err != nil {
		return nil, err
	}
	d := &openH264Decoder{}
	if r, _, _ := purego.SyscallN(openH264.createDec, uintptr(unsafe.Pointer(&d.dec))); r != 0 || d.dec == nil {
		return nil, fmt.Errorf("WelsCreateDecoder: %d", int32(r))
	}
	quiet := int32(0)
	vcall(d.dec, decSetOption, optDecTraceLevel, uintptr(unsafe.Pointer(&quiet)))
	// No error concealment: a broken picture is an error, and the viewer
	// asks for a key frame.
	p := decodingParam{targetDQ: 0xFF, propSize: 8}
	if r := vcall(d.dec, decInitialize, uintptr(unsafe.Pointer(&p))); r != 0 {
		purego.SyscallN(openH264.destroyD, uintptr(d.dec))
		return nil, fmt.Errorf("OpenH264 decoder: Initialize: %d", int64(r))
	}
	d.img = make([]byte, w*h*4)
	return d, nil
}

func (d *openH264Decoder) Decode(au []byte) ([]byte, int, int, error) {
	if len(au) == 0 {
		return nil, 0, 0, nil
	}
	var dst [3]*byte
	var info bufferInfo
	st := vcall(d.dec, decDecodeFrameNoDelay, uintptr(unsafe.Pointer(&au[0])), uintptr(len(au)),
		uintptr(unsafe.Pointer(&dst)), uintptr(unsafe.Pointer(&info)))
	runtime.KeepAlive(au)
	if info.status != 1 {
		if int32(st) != 0 {
			return nil, 0, 0, fmt.Errorf("OpenH264 decode: state %#x", int32(st))
		}
		return nil, 0, 0, nil
	}
	w, h := int(info.width), int(info.height)
	ys, cs := int(info.stride[0]), int(info.stride[1])
	if len(d.img) != w*h*4 {
		d.img = make([]byte, w*h*4)
	}
	i420ToBGRA(d.img, unsafe.Slice(dst[0], ys*h), unsafe.Slice(dst[1], cs*(h/2)), unsafe.Slice(dst[2], cs*(h/2)), ys, cs, w, h)
	return d.img, w, h, nil
}

func (d *openH264Decoder) Close() {
	if d.dec != nil {
		vcall(d.dec, decUninitialize)
		purego.SyscallN(openH264.destroyD, uintptr(d.dec))
		d.dec = nil
	}
}

// i420ToBGRA converts I420 (BT.709 limited range, as BGRAToNV12 makes it)
// to BGRA, in parallel strips.
func i420ToBGRA(dst, y, u, v []byte, ys, cs, w, h int) {
	strips := min(runtime.GOMAXPROCS(0), max(h/2, 1))
	rows := (h/2 + strips - 1) / strips
	var wg sync.WaitGroup
	for s := 0; s < strips; s++ {
		y0, y1 := s*rows*2, min((s+1)*rows*2, h)
		if y0 >= y1 {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := y0; r < y1; r++ {
				yr, ur, vr := y[r*ys:], u[(r/2)*cs:], v[(r/2)*cs:]
				d := dst[r*w*4:]
				for x := 0; x < w; x++ {
					l := (int32(yr[x]) - 16) * cY
					cu, cv := int32(ur[x/2])-128, int32(vr[x/2])-128
					o := x * 4
					d[o] = clamp8((l + cBU*cu + 32768) >> 16)
					d[o+1] = clamp8((l - cGU*cu - cGV*cv + 32768) >> 16)
					d[o+2] = clamp8((l + cRV*cv + 32768) >> 16)
					d[o+3] = 255
				}
			}
		}()
	}
	wg.Wait()
}
