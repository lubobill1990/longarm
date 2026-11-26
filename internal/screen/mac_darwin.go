//go:build darwin

package screen

// The macOS backend calls Apple's frameworks without cgo, through
// ebitengine/purego (C functions) and its objc package (Objective-C
// messages, classes made at run time, blocks). Signatures follow the
// macOS SDK headers; CF types are uintptr, structs mirror the C layout.

import (
	"fmt"
	"runtime"
	"structs"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// AppKit wants the process's main thread (RunViewer, the pattern helper):
// keep the main goroutine on it, as Go's runtime starts it there.
func init() { runtime.LockOSThread() }

type cgPoint struct {
	_    structs.HostLayout
	X, Y float64
}

type cgSize struct {
	_    structs.HostLayout
	W, H float64
}

type cgRect struct {
	_      structs.HostLayout
	Origin cgPoint
	Size   cgSize
}

// cmTime is cmTime (24 bytes: passed in memory on both ABIs).
type cmTime struct {
	_         structs.HostLayout
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

const cmTimeValid = 1

// lib is a loaded framework; the first missing symbol is kept as err (an
// older macOS) instead of panicking like purego.RegisterLibFunc.
type lib struct {
	h   uintptr
	err error
}

func openLib(path string) *lib {
	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return &lib{err: fmt.Errorf("%s: %w", path, err)}
	}
	return &lib{h: h}
}

func framework(name string) *lib {
	return openLib("/System/Library/Frameworks/" + name + ".framework/" + name)
}

func (l *lib) addr(name string) uintptr {
	if l.err != nil {
		return 0
	}
	p, err := purego.Dlsym(l.h, name)
	if err != nil {
		l.err = fmt.Errorf("%s: %w", name, err)
		return 0
	}
	return p
}

// fn binds a C function to fptr.
func (l *lib) fn(fptr any, name string) {
	if p := l.addr(name); p != 0 {
		purego.RegisterFunc(fptr, p)
	}
}

// obj reads an exported constant object (a CFStringRef key, kCFBooleanTrue).
func (l *lib) obj(name string) uintptr {
	if p := l.addr(name); p != 0 {
		return **(**uintptr)(unsafe.Pointer(&p)) // p is the variable's address
	}
	return 0
}

// The frameworks every part uses: CoreFoundation, CoreGraphics, CoreMedia,
// CoreVideo, VideoToolbox, ApplicationServices (accessibility) and the
// Objective-C runtime's autorelease pools.
var (
	cfRelease            func(uintptr)
	cfRetain             func(uintptr) uintptr
	cfNumberCreate       func(alloc uintptr, typ int, v unsafe.Pointer) uintptr
	cfNumberGetValue     func(n uintptr, typ int, v unsafe.Pointer) bool
	cfDictionaryCreate   func(alloc uintptr, keys, values unsafe.Pointer, n int, keyCB, valueCB uintptr) uintptr
	cfDictionaryGetValue func(d, key uintptr) uintptr
	cfArrayGetCount      func(a uintptr) int
	cfArrayGetValueAt    func(a uintptr, i int) uintptr
	cfBooleanGetValue    func(b uintptr) bool
	cfDataCreate         func(alloc uintptr, p unsafe.Pointer, n int) uintptr

	kCFBooleanTrue, kCFBooleanFalse      uintptr
	kCFTypeDictKeyCB, kCFTypeDictValueCB uintptr // addresses of the callback structs

	cgMainDisplayID              func() uint32
	cgGetActiveDisplayList       func(max uint32, ids *uint32, n *uint32) int32
	cgDisplayBounds              func(id uint32) cgRect
	cgDisplayCopyDisplayMode     func(id uint32) uintptr
	cgDisplayModeGetPixelWidth   func(m uintptr) uintptr
	cgDisplayModeGetPixelHeight  func(m uintptr) uintptr
	cgDisplayModeRelease         func(m uintptr)
	cgSessionCopyCurrentDict     func() uintptr
	cgEventCreate                func(src uintptr) uintptr
	cgEventGetLocation           func(ev uintptr) cgPoint
	cgEventCreateMouseEvent      func(src uintptr, typ uint32, pos cgPoint, button uint32) uintptr
	cgEventCreateScrollWheelEv2  func(src uintptr, units uint32, count uint32, w1, w2, w3 int32) uintptr
	cgEventCreateKeyboardEvent   func(src uintptr, key uint16, down bool) uintptr
	cgEventKeyboardSetUnicodeStr func(ev uintptr, n uint, s *uint16)
	cgEventSetFlags              func(ev uintptr, flags uint64)
	cgEventSetIntegerValueField  func(ev uintptr, field uint32, v int64)
	cgEventPost                  func(tap uint32, ev uintptr)
	cgColorSpaceCreateWithName   func(name uintptr) uintptr
	cgColorSpaceRelease          func(cs uintptr)
	cgDataProviderCreateWithData func(data uintptr) uintptr // cgDataProviderCreateWithCFData
	cgDataProviderRelease        func(p uintptr)
	cgImageCreate                func(w, h, bpc, bpp, bpr uintptr, cs uintptr, info uint32, provider uintptr, decode unsafe.Pointer, interpolate bool, intent int32) uintptr
	cgImageRelease               func(img uintptr)
	cgPreflightScreenCapture     func() bool // macOS 10.15+; nil before
	kCGColorSpaceSRGB            uintptr

	axIsProcessTrusted func() bool

	cmSampleBufferGetImageBuffer        func(sb uintptr) uintptr
	cmSampleBufferGetSampleAttachments  func(sb uintptr, create bool) uintptr
	cmSampleBufferGetDataBuffer         func(sb uintptr) uintptr
	cmSampleBufferGetFormatDescription  func(sb uintptr) uintptr
	cmSampleBufferCreateReady           func(alloc, data, format uintptr, nSamples, nTiming int, timing unsafe.Pointer, nSizes int, sizes *uintptr, out *uintptr) int32
	cmBlockBufferGetDataLength          func(bb uintptr) uintptr
	cmBlockBufferCopyDataBytes          func(bb, offset, n uintptr, dst unsafe.Pointer) int32
	cmBlockBufferCreateWithMemoryBlock  func(alloc, mem, blockLen, blockAlloc, custom, offset, dataLen uintptr, flags uint32, out *uintptr) int32
	cmBlockBufferReplaceDataBytes       func(src unsafe.Pointer, bb, offset, n uintptr) int32
	cmVideoFormatDescGetH264ParamSet    func(fd, index uintptr, p **byte, size *uintptr, count *uintptr, nalLen *int32) int32
	cmVideoFormatDescCreateFromH264Sets func(alloc, n uintptr, ptrs unsafe.Pointer, sizes unsafe.Pointer, nalLen int32, out *uintptr) int32
	kCMSampleAttachmentKey_NotSync      uintptr

	cvPixelBufferCreate              func(alloc, w, h uintptr, format uint32, attrs uintptr, out *uintptr) int32
	cvPixelBufferPoolCreatePixelBuf  func(alloc, pool uintptr, out *uintptr) int32
	cvPixelBufferLockBaseAddress     func(pb uintptr, flags uint64) int32
	cvPixelBufferUnlockBaseAddress   func(pb uintptr, flags uint64) int32
	cvPixelBufferGetBaseAddress      func(pb uintptr) unsafe.Pointer
	cvPixelBufferGetBytesPerRow      func(pb uintptr) uintptr
	cvPixelBufferGetWidth            func(pb uintptr) uintptr
	cvPixelBufferGetHeight           func(pb uintptr) uintptr
	cvPixelBufferRelease             func(pb uintptr)
	kCVPixelBufferPixelFormatTypeKey uintptr
	kCVPixelBufferWidthKey           uintptr
	kCVPixelBufferHeightKey          uintptr
	kCVPixelBufferIOSurfacePropsKey  uintptr

	vtCompressionSessionCreate          func(alloc uintptr, w, h int32, codec uint32, spec, srcAttrs, dataAlloc, cb, refcon uintptr, out *uintptr) int32
	vtSessionSetProperty                func(s, key, value uintptr) int32
	vtSessionCopyProperty               func(s, key, alloc uintptr, out *uintptr) int32
	vtCompressionSessionPrepare         func(s uintptr) int32
	vtCompressionSessionGetPixelBufPool func(s uintptr) uintptr
	vtCompressionSessionEncodeFrame     func(s, pb uintptr, pts, dur cmTime, props, refcon uintptr, flags *uint32) int32
	vtCompressionSessionCompleteFrames  func(s uintptr, until cmTime) int32
	vtCompressionSessionInvalidate      func(s uintptr)
	vtDecompressionSessionCreate        func(alloc, format, spec, dstAttrs uintptr, cb unsafe.Pointer, out *uintptr) int32
	vtDecompressionSessionDecodeFrame   func(s, sb uintptr, flags uint32, refcon uintptr, info *uint32) int32
	vtDecompressionSessionWaitAsync     func(s uintptr) int32
	vtDecompressionSessionCanAccept     func(s, format uintptr) bool
	vtDecompressionSessionInvalidate    func(s uintptr)

	kVTCompressionPropertyKey_RealTime             uintptr
	kVTCompressionPropertyKey_ProfileLevel         uintptr
	kVTCompressionPropertyKey_AverageBitRate       uintptr
	kVTCompressionPropertyKey_AllowFrameReordering uintptr
	kVTCompressionPropertyKey_MaxKeyFrameInterval  uintptr
	kVTCompressionPropertyKey_ExpectedFrameRate    uintptr
	kVTCompressionPropertyKey_UsingHWEncoder       uintptr
	kVTProfileLevel_H264_High_AutoLevel            uintptr
	kVTEncodeFrameOptionKey_ForceKeyFrame          uintptr
	kVTVideoEncoderSpec_EnableHWEncoder            uintptr

	objcPoolPush        func() uintptr
	objcPoolPop         func(uintptr)
	dispatchQueueCreate func(label string, attr uintptr) uintptr
	dispatchRelease     func(q uintptr)
)

var (
	macOnce sync.Once
	macErr  error
)

// macLoad binds the frameworks (once); an error means this macOS lacks one.
func macLoad() error {
	macOnce.Do(func() {
		cf := framework("CoreFoundation")
		cf.fn(&cfRelease, "CFRelease")
		cf.fn(&cfRetain, "CFRetain")
		cf.fn(&cfNumberCreate, "CFNumberCreate")
		cf.fn(&cfNumberGetValue, "CFNumberGetValue")
		cf.fn(&cfDictionaryCreate, "CFDictionaryCreate")
		cf.fn(&cfDictionaryGetValue, "CFDictionaryGetValue")
		cf.fn(&cfArrayGetCount, "CFArrayGetCount")
		cf.fn(&cfArrayGetValueAt, "CFArrayGetValueAtIndex")
		cf.fn(&cfBooleanGetValue, "CFBooleanGetValue")
		cf.fn(&cfDataCreate, "CFDataCreate")
		kCFBooleanTrue, kCFBooleanFalse = cf.obj("kCFBooleanTrue"), cf.obj("kCFBooleanFalse")
		kCFTypeDictKeyCB, kCFTypeDictValueCB = cf.addr("kCFTypeDictionaryKeyCallBacks"), cf.addr("kCFTypeDictionaryValueCallBacks")

		cg := framework("CoreGraphics")
		cg.fn(&cgMainDisplayID, "CGMainDisplayID")
		cg.fn(&cgGetActiveDisplayList, "CGGetActiveDisplayList")
		cg.fn(&cgDisplayBounds, "CGDisplayBounds")
		cg.fn(&cgDisplayCopyDisplayMode, "CGDisplayCopyDisplayMode")
		cg.fn(&cgDisplayModeGetPixelWidth, "CGDisplayModeGetPixelWidth")
		cg.fn(&cgDisplayModeGetPixelHeight, "CGDisplayModeGetPixelHeight")
		cg.fn(&cgDisplayModeRelease, "CGDisplayModeRelease")
		cg.fn(&cgSessionCopyCurrentDict, "CGSessionCopyCurrentDictionary")
		cg.fn(&cgEventCreate, "CGEventCreate")
		cg.fn(&cgEventGetLocation, "CGEventGetLocation")
		cg.fn(&cgEventCreateMouseEvent, "CGEventCreateMouseEvent")
		cg.fn(&cgEventCreateScrollWheelEv2, "CGEventCreateScrollWheelEvent2")
		cg.fn(&cgEventCreateKeyboardEvent, "CGEventCreateKeyboardEvent")
		cg.fn(&cgEventKeyboardSetUnicodeStr, "CGEventKeyboardSetUnicodeString")
		cg.fn(&cgEventSetFlags, "CGEventSetFlags")
		cg.fn(&cgEventSetIntegerValueField, "CGEventSetIntegerValueField")
		cg.fn(&cgEventPost, "CGEventPost")
		cg.fn(&cgColorSpaceCreateWithName, "CGColorSpaceCreateWithName")
		cg.fn(&cgColorSpaceRelease, "CGColorSpaceRelease")
		cg.fn(&cgDataProviderCreateWithData, "CGDataProviderCreateWithCFData")
		cg.fn(&cgDataProviderRelease, "CGDataProviderRelease")
		cg.fn(&cgImageCreate, "CGImageCreate")
		cg.fn(&cgImageRelease, "CGImageRelease")
		kCGColorSpaceSRGB = cg.obj("kCGColorSpaceSRGB")
		if cg.err == nil {
			if p, err := purego.Dlsym(cg.h, "CGPreflightScreenCaptureAccess"); err == nil {
				purego.RegisterFunc(&cgPreflightScreenCapture, p)
			}
		}

		as := framework("ApplicationServices")
		as.fn(&axIsProcessTrusted, "AXIsProcessTrusted")

		cm := framework("CoreMedia")
		cm.fn(&cmSampleBufferGetImageBuffer, "CMSampleBufferGetImageBuffer")
		cm.fn(&cmSampleBufferGetSampleAttachments, "CMSampleBufferGetSampleAttachmentsArray")
		cm.fn(&cmSampleBufferGetDataBuffer, "CMSampleBufferGetDataBuffer")
		cm.fn(&cmSampleBufferGetFormatDescription, "CMSampleBufferGetFormatDescription")
		cm.fn(&cmSampleBufferCreateReady, "CMSampleBufferCreateReady")
		cm.fn(&cmBlockBufferGetDataLength, "CMBlockBufferGetDataLength")
		cm.fn(&cmBlockBufferCopyDataBytes, "CMBlockBufferCopyDataBytes")
		cm.fn(&cmBlockBufferCreateWithMemoryBlock, "CMBlockBufferCreateWithMemoryBlock")
		cm.fn(&cmBlockBufferReplaceDataBytes, "CMBlockBufferReplaceDataBytes")
		cm.fn(&cmVideoFormatDescGetH264ParamSet, "CMVideoFormatDescriptionGetH264ParameterSetAtIndex")
		cm.fn(&cmVideoFormatDescCreateFromH264Sets, "CMVideoFormatDescriptionCreateFromH264ParameterSets")
		kCMSampleAttachmentKey_NotSync = cm.obj("kCMSampleAttachmentKey_NotSync")

		cv := framework("CoreVideo")
		cv.fn(&cvPixelBufferCreate, "CVPixelBufferCreate")
		cv.fn(&cvPixelBufferPoolCreatePixelBuf, "CVPixelBufferPoolCreatePixelBuffer")
		cv.fn(&cvPixelBufferLockBaseAddress, "CVPixelBufferLockBaseAddress")
		cv.fn(&cvPixelBufferUnlockBaseAddress, "CVPixelBufferUnlockBaseAddress")
		cv.fn(&cvPixelBufferGetBaseAddress, "CVPixelBufferGetBaseAddress")
		cv.fn(&cvPixelBufferGetBytesPerRow, "CVPixelBufferGetBytesPerRow")
		cv.fn(&cvPixelBufferGetWidth, "CVPixelBufferGetWidth")
		cv.fn(&cvPixelBufferGetHeight, "CVPixelBufferGetHeight")
		cv.fn(&cvPixelBufferRelease, "CVPixelBufferRelease")
		kCVPixelBufferPixelFormatTypeKey = cv.obj("kCVPixelBufferPixelFormatTypeKey")
		kCVPixelBufferWidthKey = cv.obj("kCVPixelBufferWidthKey")
		kCVPixelBufferHeightKey = cv.obj("kCVPixelBufferHeightKey")
		kCVPixelBufferIOSurfacePropsKey = cv.obj("kCVPixelBufferIOSurfacePropertiesKey")

		vt := framework("VideoToolbox")
		vt.fn(&vtCompressionSessionCreate, "VTCompressionSessionCreate")
		vt.fn(&vtSessionSetProperty, "VTSessionSetProperty")
		vt.fn(&vtSessionCopyProperty, "VTSessionCopyProperty")
		vt.fn(&vtCompressionSessionPrepare, "VTCompressionSessionPrepareToEncodeFrames")
		vt.fn(&vtCompressionSessionGetPixelBufPool, "VTCompressionSessionGetPixelBufferPool")
		vt.fn(&vtCompressionSessionEncodeFrame, "VTCompressionSessionEncodeFrame")
		vt.fn(&vtCompressionSessionCompleteFrames, "VTCompressionSessionCompleteFrames")
		vt.fn(&vtCompressionSessionInvalidate, "VTCompressionSessionInvalidate")
		vt.fn(&vtDecompressionSessionCreate, "VTDecompressionSessionCreate")
		vt.fn(&vtDecompressionSessionDecodeFrame, "VTDecompressionSessionDecodeFrame")
		vt.fn(&vtDecompressionSessionWaitAsync, "VTDecompressionSessionWaitForAsynchronousFrames")
		vt.fn(&vtDecompressionSessionCanAccept, "VTDecompressionSessionCanAcceptFormatDescription")
		vt.fn(&vtDecompressionSessionInvalidate, "VTDecompressionSessionInvalidate")
		kVTCompressionPropertyKey_RealTime = vt.obj("kVTCompressionPropertyKey_RealTime")
		kVTCompressionPropertyKey_ProfileLevel = vt.obj("kVTCompressionPropertyKey_ProfileLevel")
		kVTCompressionPropertyKey_AverageBitRate = vt.obj("kVTCompressionPropertyKey_AverageBitRate")
		kVTCompressionPropertyKey_AllowFrameReordering = vt.obj("kVTCompressionPropertyKey_AllowFrameReordering")
		kVTCompressionPropertyKey_MaxKeyFrameInterval = vt.obj("kVTCompressionPropertyKey_MaxKeyFrameInterval")
		kVTCompressionPropertyKey_ExpectedFrameRate = vt.obj("kVTCompressionPropertyKey_ExpectedFrameRate")
		kVTCompressionPropertyKey_UsingHWEncoder = vt.obj("kVTCompressionPropertyKey_UsingHardwareAcceleratedVideoEncoder")
		kVTProfileLevel_H264_High_AutoLevel = vt.obj("kVTProfileLevel_H264_High_AutoLevel")
		kVTEncodeFrameOptionKey_ForceKeyFrame = vt.obj("kVTEncodeFrameOptionKey_ForceKeyFrame")
		kVTVideoEncoderSpec_EnableHWEncoder = vt.obj("kVTVideoEncoderSpecification_EnableHardwareAcceleratedVideoEncoder")

		lo := openLib("/usr/lib/libobjc.A.dylib")
		lo.fn(&objcPoolPush, "objc_autoreleasePoolPush")
		lo.fn(&objcPoolPop, "objc_autoreleasePoolPop")
		sys := openLib("/usr/lib/libSystem.B.dylib")
		sys.fn(&dispatchQueueCreate, "dispatch_queue_create")
		sys.fn(&dispatchRelease, "dispatch_release")

		for _, l := range []*lib{cf, cg, as, cm, cv, vt, lo, sys} {
			if l.err != nil {
				macErr = l.err
				return
			}
		}
	})
	return macErr
}

// withPool runs f in an autorelease pool (threads other than AppKit's
// main loop have none, and Foundation's convenience constructors
// autorelease). A pool belongs to its thread: the goroutine stays on it.
func withPool(f func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p := objcPoolPush()
	defer objcPoolPop(p)
	f()
}

var sels sync.Map // string → objc.SEL

func sel(name string) objc.SEL {
	if s, ok := sels.Load(name); ok {
		return s.(objc.SEL)
	}
	s := objc.RegisterName(name)
	sels.Store(name, s)
	return s
}

func class(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

// send sends a message returning an object (or nothing).
func send(id objc.ID, name string, args ...any) objc.ID { return id.Send(sel(name), args...) }

// sendT sends a message returning T (BOOL, NSInteger, a struct).
func sendT[T any](id objc.ID, name string, args ...any) T {
	return objc.Send[T](id, sel(name), args...)
}

// nsString makes an autoreleased NSString.
func nsString(s string) objc.ID {
	return send(class("NSString"), "stringWithUTF8String:", s+"\x00")
}

// goString reads an NSString.
func goString(s objc.ID) string {
	if s == 0 {
		return ""
	}
	return sendT[string](s, "UTF8String")
}

// nsError describes an NSError.
func nsError(e objc.ID) string {
	if e == 0 {
		return "unknown error"
	}
	return goString(send(e, "localizedDescription"))
}

// cfNumber32 makes a cfNumber of an int32 (release it).
func cfNumber32(v int32) uintptr {
	const kCFNumberSInt32Type = 3
	return cfNumberCreate(0, kCFNumberSInt32Type, unsafe.Pointer(&v))
}

// cfDict makes a cfDictionary of CF keys and values (release it).
func cfDict(kv ...uintptr) uintptr {
	n := len(kv) / 2
	keys, values := make([]uintptr, n), make([]uintptr, n)
	for i := 0; i < n; i++ {
		keys[i], values[i] = kv[2*i], kv[2*i+1]
	}
	if n == 0 {
		return cfDictionaryCreate(0, nil, nil, 0, kCFTypeDictKeyCB, kCFTypeDictValueCB)
	}
	d := cfDictionaryCreate(0, unsafe.Pointer(&keys[0]), unsafe.Pointer(&values[0]), n, kCFTypeDictKeyCB, kCFTypeDictValueCB)
	runtime.KeepAlive(keys)
	runtime.KeepAlive(values)
	return d
}

// fourCC is a pixel format or codec code ('BGRA', 'avc1').
func fourCC(s string) uint32 {
	return uint32(s[0])<<24 | uint32(s[1])<<16 | uint32(s[2])<<8 | uint32(s[3])
}

// copyPixels copies a locked BGRA cvPixelBuffer into dst (w*h*4, stride
// w*4), cropping or leaving the rest when the sizes differ.
func copyPixels(dst []byte, w, h int, pb uintptr) {
	base := cvPixelBufferGetBaseAddress(pb)
	if base == nil {
		return
	}
	bpr := int(cvPixelBufferGetBytesPerRow(pb))
	pw, ph := int(cvPixelBufferGetWidth(pb)), int(cvPixelBufferGetHeight(pb))
	src := unsafe.Slice((*byte)(base), bpr*ph)
	rowLen := min(w, pw) * 4
	for y := 0; y < min(h, ph); y++ {
		copy(dst[y*w*4:y*w*4+rowLen], src[y*bpr:y*bpr+rowLen])
	}
}
