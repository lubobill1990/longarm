//go:build darwin

package screen

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"structs"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// VideoToolbox's output callbacks are C function pointers (made once:
// purego's callbacks are never freed); their refcon is an ID in these
// maps, as C may not hold Go pointers.
var (
	vtCallbacks sync.Once
	vtEncCB     uintptr
	vtDecCB     uintptr
	vtSessions  sync.Map // refcon → *vtEncoder | *vtDecoder
	vtNextID    atomic.Uintptr
)

func vtInit() {
	vtCallbacks.Do(func() {
		vtEncCB = purego.NewCallback(vtEncoded)
		// VTDecompressionOutputCallback also gets the presentation time and
		// duration (CMTime, in memory after these on both ABIs): unused.
		vtDecCB = purego.NewCallback(vtDecoded)
	})
}

// vtEncoder is VTCompressionSession: BGRA pixel buffers in, AVCC sample
// buffers out (turned into Annex B), one frame at a time.
type vtEncoder struct {
	id    uintptr
	sess  uintptr
	w, h  int
	name  string
	start time.Time
	last  int64 // the last frame's time stamp (µs)
	force bool

	mu  sync.Mutex // the output callback runs on VideoToolbox's thread
	out []byte
	err error
}

func newH264Encoder(o EncoderOptions) (VideoEncoder, error) {
	if err := macLoad(); err != nil {
		return nil, err
	}
	vtInit()
	fps := max(o.FPS, 1)
	e := &vtEncoder{id: vtNextID.Add(1), w: o.Width, h: o.Height, start: time.Now(), last: -1}
	var spec uintptr
	if o.Software {
		spec = cfDict(kVTVideoEncoderSpec_EnableHWEncoder, kCFBooleanFalse)
		defer cfRelease(spec)
	}
	attrs := pixelBufferAttrs(o.Width, o.Height)
	defer cfRelease(attrs)
	vtSessions.Store(e.id, e)
	if st := vtCompressionSessionCreate(0, int32(o.Width), int32(o.Height), fourCC("avc1"), spec, attrs, 0, vtEncCB, e.id, &e.sess); st != 0 {
		vtSessions.Delete(e.id)
		return nil, fmt.Errorf("VTCompressionSessionCreate: OSStatus %d", st)
	}
	set := func(key, value uintptr) {
		if st := vtSessionSetProperty(e.sess, key, value); st != 0 && e.err == nil {
			e.err = fmt.Errorf("VTSessionSetProperty: OSStatus %d", st)
		}
	}
	setInt := func(key uintptr, v int) {
		n := cfNumber32(int32(v))
		set(key, n)
		cfRelease(n)
	}
	set(kVTCompressionPropertyKey_RealTime, kCFBooleanTrue)
	set(kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_High_AutoLevel)
	set(kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse) // no B frames: a frame out per frame in
	setInt(kVTCompressionPropertyKey_AverageBitRate, o.Bitrate)
	setInt(kVTCompressionPropertyKey_MaxKeyFrameInterval, fps*10) // a key frame every ~10 s of motion; more on request
	setInt(kVTCompressionPropertyKey_ExpectedFrameRate, fps)
	if e.err != nil {
		err := e.err
		e.Close()
		return nil, err
	}
	vtCompressionSessionPrepare(e.sess)
	e.name = "hardware"
	var hw uintptr
	if vtSessionCopyProperty(e.sess, kVTCompressionPropertyKey_UsingHWEncoder, 0, &hw) == 0 && hw != 0 {
		if !cfBooleanGetValue(hw) {
			e.name = "software"
		}
		cfRelease(hw)
	}
	return e, nil
}

// pixelBufferAttrs: BGRA w×h, IOSurface-backed (what the hardware takes
// without a copy).
func pixelBufferAttrs(w, h int) uintptr {
	f, wn, hn, io := cfNumber32(int32(fourCC("BGRA"))), cfNumber32(int32(w)), cfNumber32(int32(h)), cfDict()
	defer func() { cfRelease(f); cfRelease(wn); cfRelease(hn); cfRelease(io) }()
	return cfDict(kCVPixelBufferPixelFormatTypeKey, f, kCVPixelBufferWidthKey, wn, kCVPixelBufferHeightKey, hn, kCVPixelBufferIOSurfacePropsKey, io)
}

func (e *vtEncoder) Name() string   { return e.name }
func (e *vtEncoder) ForceKeyframe() { e.force = true }

func (e *vtEncoder) Close() {
	if e.sess != 0 {
		vtCompressionSessionInvalidate(e.sess)
		cfRelease(e.sess)
		e.sess = 0
	}
	vtSessions.Delete(e.id)
}

func (e *vtEncoder) Encode(bgra []byte) ([]byte, error) {
	if len(bgra) < e.w*e.h*4 {
		return nil, fmt.Errorf("encode: a frame of %d bytes for %dx%d", len(bgra), e.w, e.h)
	}
	var pb uintptr
	if pool := vtCompressionSessionGetPixelBufPool(e.sess); pool == 0 || cvPixelBufferPoolCreatePixelBuf(0, pool, &pb) != 0 {
		attrs := pixelBufferAttrs(e.w, e.h)
		st := cvPixelBufferCreate(0, uintptr(e.w), uintptr(e.h), fourCC("BGRA"), attrs, &pb)
		cfRelease(attrs)
		if st != 0 {
			return nil, fmt.Errorf("CVPixelBufferCreate: CVReturn %d", st)
		}
	}
	defer cvPixelBufferRelease(pb)
	if st := cvPixelBufferLockBaseAddress(pb, 0); st != 0 {
		return nil, fmt.Errorf("CVPixelBufferLockBaseAddress: CVReturn %d", st)
	}
	base, bpr := cvPixelBufferGetBaseAddress(pb), int(cvPixelBufferGetBytesPerRow(pb))
	dst := unsafe.Slice((*byte)(base), bpr*e.h)
	for y := 0; y < e.h; y++ {
		copy(dst[y*bpr:y*bpr+e.w*4], bgra[y*e.w*4:])
	}
	cvPixelBufferUnlockBaseAddress(pb, 0)

	var props uintptr
	if e.force {
		props = cfDict(kVTEncodeFrameOptionKey_ForceKeyFrame, kCFBooleanTrue)
		defer cfRelease(props)
		e.force = false
	}
	// Time stamps from the clock (frames come when the screen changes), so
	// rate control sees the real frame rate.
	ts := max(time.Since(e.start).Microseconds(), e.last+1)
	e.last = ts
	pts := cmTime{Value: ts, Timescale: 1_000_000, Flags: cmTimeValid}
	var flags uint32
	st := vtCompressionSessionEncodeFrame(e.sess, pb, pts, cmTime{}, props, 0, &flags)
	if st == 0 {
		// Synchronous for the caller: the callback has run when this returns.
		st = vtCompressionSessionCompleteFrames(e.sess, cmTime{})
	}
	e.mu.Lock()
	out, err := e.out, e.err
	e.out, e.err = nil, nil
	e.mu.Unlock()
	if st != 0 {
		return nil, fmt.Errorf("VTCompressionSessionEncodeFrame: OSStatus %d", st)
	}
	return out, err
}

// vtEncoded is the VTCompressionOutputCallback.
func vtEncoded(refcon, _ uintptr, status int32, _ uint32, sb uintptr) {
	v, ok := vtSessions.Load(refcon)
	if !ok {
		return
	}
	e := v.(*vtEncoder)
	e.mu.Lock()
	defer e.mu.Unlock()
	if status != 0 {
		e.err = fmt.Errorf("VideoToolbox encode: OSStatus %d", status)
		return
	}
	if sb == 0 {
		return // the encoder dropped the frame
	}
	key := true
	if att := cmSampleBufferGetSampleAttachments(sb, false); att != 0 && cfArrayGetCount(att) > 0 {
		if ns := cfDictionaryGetValue(cfArrayGetValueAt(att, 0), kCMSampleAttachmentKey_NotSync); ns != 0 && cfBooleanGetValue(ns) {
			key = false
		}
	}
	fd := cmSampleBufferGetFormatDescription(sb)
	var count uintptr
	nalLen := int32(4)
	cmVideoFormatDescGetH264ParamSet(fd, 0, nil, nil, &count, &nalLen)
	if key {
		// SPS and PPS travel in the format description: a key frame
		// carries them in band, as Annex B decoders expect.
		for i := uintptr(0); i < count; i++ {
			var p *byte
			var n uintptr
			if cmVideoFormatDescGetH264ParamSet(fd, i, &p, &n, nil, nil) == 0 && p != nil {
				e.out = append(e.out, 0, 0, 0, 1)
				e.out = append(e.out, unsafe.Slice(p, n)...)
			}
		}
	}
	bb := cmSampleBufferGetDataBuffer(sb)
	if bb == 0 {
		return
	}
	n := cmBlockBufferGetDataLength(bb)
	if n == 0 {
		return
	}
	data := make([]byte, n)
	if st := cmBlockBufferCopyDataBytes(bb, 0, n, unsafe.Pointer(&data[0])); st != 0 {
		e.err = fmt.Errorf("CMBlockBufferCopyDataBytes: OSStatus %d", st)
		return
	}
	var good bool
	if e.out, good = appendAnnexB(e.out, data, int(nalLen)); !good {
		e.err = errors.New("VideoToolbox gave a malformed access unit")
	}
}

// vtDecoder is VTDecompressionSession: Annex B access units in (SPS and
// PPS make the format description), BGRA pixel buffers out.
type vtDecoder struct {
	id       uintptr
	w, h     int
	rec      vtDecRecord
	sess     uintptr
	format   uintptr
	sps, pps []byte
	img      []byte

	mu  sync.Mutex
	got bool
	err error
}

// vtDecRecord is VTDecompressionOutputCallbackRecord.
type vtDecRecord struct {
	_      structs.HostLayout
	cb     uintptr
	refcon uintptr
}

func newH264Decoder(w, h int) (VideoDecoder, error) {
	if err := macLoad(); err != nil {
		return nil, err
	}
	vtInit()
	d := &vtDecoder{id: vtNextID.Add(1), w: w, h: h, img: make([]byte, w*h*4)}
	d.rec = vtDecRecord{cb: vtDecCB, refcon: d.id}
	vtSessions.Store(d.id, d)
	return d, nil
}

func (d *vtDecoder) closeSession() {
	if d.sess != 0 {
		vtDecompressionSessionInvalidate(d.sess)
		cfRelease(d.sess)
		d.sess = 0
	}
}

func (d *vtDecoder) Close() {
	d.closeSession()
	if d.format != 0 {
		cfRelease(d.format)
		d.format = 0
	}
	vtSessions.Delete(d.id)
}

// setParams makes the format description of new SPS/PPS, and a session
// for it unless the old one takes it.
func (d *vtDecoder) setParams() error {
	ptrs := [2]*byte{&d.sps[0], &d.pps[0]}
	sizes := [2]uintptr{uintptr(len(d.sps)), uintptr(len(d.pps))}
	var f uintptr
	st := cmVideoFormatDescCreateFromH264Sets(0, 2, unsafe.Pointer(&ptrs[0]), unsafe.Pointer(&sizes[0]), 4, &f)
	runtime.KeepAlive(ptrs)
	if st != 0 {
		return fmt.Errorf("CMVideoFormatDescriptionCreateFromH264ParameterSets: OSStatus %d", st)
	}
	if d.format != 0 {
		cfRelease(d.format)
	}
	d.format = f
	if d.sess != 0 && vtDecompressionSessionCanAccept(d.sess, f) {
		return nil
	}
	d.closeSession()
	fmtNum := cfNumber32(int32(fourCC("BGRA")))
	attrs := cfDict(kCVPixelBufferPixelFormatTypeKey, fmtNum)
	defer func() { cfRelease(fmtNum); cfRelease(attrs) }()
	if st := vtDecompressionSessionCreate(0, f, 0, attrs, unsafe.Pointer(&d.rec), &d.sess); st != 0 {
		d.sess = 0
		return fmt.Errorf("VTDecompressionSessionCreate: OSStatus %d", st)
	}
	return nil
}

func (d *vtDecoder) Decode(au []byte) ([]byte, int, int, error) {
	var vcl [][]byte
	changed := false
	for _, n := range annexBNALs(au) {
		switch n[0] & 0x1F {
		case 7: // SPS
			if !bytes.Equal(n, d.sps) {
				d.sps, changed = slices.Clone(n), true
			}
		case 8: // PPS
			if !bytes.Equal(n, d.pps) {
				d.pps, changed = slices.Clone(n), true
			}
		case 9: // access unit delimiter
		default:
			vcl = append(vcl, n)
		}
	}
	if changed && d.sps != nil && d.pps != nil {
		if err := d.setParams(); err != nil {
			return nil, 0, 0, err
		}
	}
	if d.sess == 0 || len(vcl) == 0 {
		return nil, 0, 0, nil // no key frame yet
	}
	data := appendAVCC(nil, vcl...)
	var bb, sb uintptr
	if st := cmBlockBufferCreateWithMemoryBlock(0, 0, uintptr(len(data)), 0, 0, 0, uintptr(len(data)), 0, &bb); st != 0 {
		return nil, 0, 0, fmt.Errorf("CMBlockBufferCreateWithMemoryBlock: OSStatus %d", st)
	}
	defer cfRelease(bb)
	if st := cmBlockBufferReplaceDataBytes(unsafe.Pointer(&data[0]), bb, 0, uintptr(len(data))); st != 0 {
		return nil, 0, 0, fmt.Errorf("CMBlockBufferReplaceDataBytes: OSStatus %d", st)
	}
	size := uintptr(len(data))
	if st := cmSampleBufferCreateReady(0, bb, d.format, 1, 0, nil, 1, &size, &sb); st != 0 {
		return nil, 0, 0, fmt.Errorf("CMSampleBufferCreateReady: OSStatus %d", st)
	}
	defer cfRelease(sb)
	d.mu.Lock()
	d.got, d.err = false, nil
	d.mu.Unlock()
	var info uint32
	st := vtDecompressionSessionDecodeFrame(d.sess, sb, 0, 0, &info) // 0: synchronous
	vtDecompressionSessionWaitAsync(d.sess)
	if st != 0 {
		return nil, 0, 0, fmt.Errorf("VTDecompressionSessionDecodeFrame: OSStatus %d", st)
	}
	d.mu.Lock()
	got, err := d.got, d.err
	d.mu.Unlock()
	if err != nil {
		return nil, 0, 0, err
	}
	if !got {
		return nil, 0, 0, nil
	}
	return d.img, d.w, d.h, nil
}

// vtDecoded is the VTDecompressionOutputCallback (its CMTime arguments
// left out).
func vtDecoded(refcon, _ uintptr, status int32, _ uint32, img uintptr) {
	v, ok := vtSessions.Load(refcon)
	if !ok {
		return
	}
	d := v.(*vtDecoder)
	d.mu.Lock()
	defer d.mu.Unlock()
	if status != 0 {
		d.err = fmt.Errorf("VideoToolbox decode: OSStatus %d", status)
		return
	}
	if img == 0 {
		return
	}
	const readOnly = 1
	if cvPixelBufferLockBaseAddress(img, readOnly) != 0 {
		return
	}
	copyPixels(d.img, d.w, d.h, img)
	cvPixelBufferUnlockBaseAddress(img, readOnly)
	d.got = true
}
