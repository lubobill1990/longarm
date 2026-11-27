//go:build windows

package screen

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WASAPI in shared mode: loopback capture of the default speakers, and
// playback with the audio engine converting from our 16-bit PCM
// (AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM, Windows 10+).

var (
	procCoCreateInstance = modOle32.NewProc("CoCreateInstance")

	clsidMMDeviceEnumerator = guid(0xBCDE0395, 0xE52F, 0x467C, 0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E)
	iidIMMDeviceEnumerator  = guid(0xA95664D2, 0x9614, 0x4F35, 0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6)
	iidIAudioClient         = guid(0x1CB9AD4C, 0xDBFA, 0x4C32, 0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2)
	iidIAudioCaptureClient  = guid(0xC8ADBD64, 0xE71E, 0x48A0, 0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17)
	iidIAudioRenderClient   = guid(0xF294ACFC, 0x3146, 0x4483, 0xA7, 0xBF, 0xAD, 0xDC, 0xA7, 0xC2, 0x60, 0xE2)
	subtypeIEEEFloat        = guid(0x00000003, 0x0000, 0x0010, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71)
)

const (
	slotEnumGetDefaultEndpoint = 4  // IMMDeviceEnumerator::GetDefaultAudioEndpoint
	slotDeviceActivate         = 3  // IMMDevice::Activate
	slotACInitialize           = 3  // IAudioClient::Initialize
	slotACGetBufferSize        = 4  //   ::GetBufferSize
	slotACGetCurrentPadding    = 6  //   ::GetCurrentPadding
	slotACGetMixFormat         = 8  //   ::GetMixFormat
	slotACStart                = 10 //   ::Start
	slotACStop                 = 11 //   ::Stop
	slotACGetService           = 14 //   ::GetService
	slotBufGet                 = 3  // IAudioCaptureClient / IAudioRenderClient ::GetBuffer
	slotBufRelease             = 4  //   ::ReleaseBuffer
	slotCapNextPacketSize      = 5  // IAudioCaptureClient::GetNextPacketSize

	clsctxAll          = 0x17
	eRender, eConsole  = 0, 0
	streamLoopback     = 0x00020000
	streamAutoConvert  = 0x80000000 | 0x08000000 // AUTOCONVERTPCM | SRC_DEFAULT_QUALITY
	bufferFlagsSilent  = 0x2
	waveFormatPCM      = 1
	waveFormatFloat    = 3
	waveFormatExtended = 0xFFFE
	hns                = 10_000_000 // 100-ns units a second
)

// waveFormat is WAVEFORMATEXTENSIBLE (WAVEFORMATEX and what follows it).
type waveFormat struct {
	Tag         uint16
	Channels    uint16
	Rate        uint32
	AvgBytes    uint32
	BlockAlign  uint16
	Bits        uint16
	Size        uint16
	ValidBits   uint16
	ChannelMask uint32
	SubFormat   windows.GUID
}

// audioClient opens the default speakers' IAudioClient.
func audioClient() (*comObj, error) {
	procCoInitializeEx.Call(0, 0)
	var en, dev, ac *comObj
	if err := hr("CoCreateInstance(MMDeviceEnumerator)", callProc(procCoCreateInstance, uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&en)))); err != nil {
		return nil, err
	}
	defer release(en)
	if err := hr("GetDefaultAudioEndpoint", comCall(en, slotEnumGetDefaultEndpoint, eRender, eConsole, uintptr(unsafe.Pointer(&dev)))); err != nil {
		return nil, fmt.Errorf("no speakers: %w", err)
	}
	defer release(dev)
	if err := hr("Activate(IAudioClient)", comCall(dev, slotDeviceActivate, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&ac)))); err != nil {
		return nil, err
	}
	return ac, nil
}

func callProc(p *windows.LazyProc, args ...uintptr) uintptr {
	r, _, _ := p.Call(args...)
	return r
}

type wasapiCapture struct {
	ac, cc *comObj
	mix    waveFormat
	float  bool
	inCh   int
	f      AudioFormat
	mu     sync.Mutex
	closed bool
}

func openAudioCapture() (AudioCapture, error) {
	ac, err := audioClient()
	if err != nil {
		return nil, err
	}
	c := &wasapiCapture{ac: ac}
	var mix *waveFormat
	if err := hr("GetMixFormat", comCall(ac, slotACGetMixFormat, uintptr(unsafe.Pointer(&mix)))); err != nil {
		release(ac)
		return nil, err
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(mix)))
	c.mix = *mix
	if mix.Tag != waveFormatExtended {
		c.mix.SubFormat = windows.GUID{}
	}
	c.float = mix.Tag == waveFormatFloat || mix.Tag == waveFormatExtended && mix.SubFormat == subtypeIEEEFloat
	if !c.float && mix.Bits != 16 && mix.Bits != 32 {
		release(ac)
		return nil, fmt.Errorf("speakers' mix format: %d-bit PCM", mix.Bits)
	}
	c.inCh = int(mix.Channels)
	c.f = AudioFormat{Rate: int(mix.Rate), Channels: min(c.inCh, 2)}
	if err := hr("IAudioClient::Initialize(loopback)", comCall(ac, slotACInitialize, 0, streamLoopback, hns/5, 0, uintptr(unsafe.Pointer(mix)), 0)); err != nil {
		release(ac)
		return nil, err
	}
	if err := hr("GetService(IAudioCaptureClient)", comCall(ac, slotACGetService, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&c.cc)))); err != nil {
		release(ac)
		return nil, err
	}
	if err := hr("IAudioClient::Start", comCall(ac, slotACStart)); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *wasapiCapture) Format() AudioFormat { return c.f }

// Read polls the capture buffer every 10 ms and returns what came
// (loopback capture has no event while nothing plays).
func (c *wasapiCapture) Read() ([]int16, error) {
	var out []int16
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, errors.New("audio capture closed")
		}
		for {
			var n uint32
			if err := hr("GetNextPacketSize", comCall(c.cc, slotCapNextPacketSize, uintptr(unsafe.Pointer(&n)))); err != nil {
				c.mu.Unlock()
				return nil, err
			}
			if n == 0 {
				break
			}
			var data unsafe.Pointer
			var frames, flags uint32
			if err := hr("IAudioCaptureClient::GetBuffer", comCall(c.cc, slotBufGet, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&frames)), uintptr(unsafe.Pointer(&flags)), 0, 0)); err != nil {
				c.mu.Unlock()
				return nil, err
			}
			out = c.convert(out, data, int(frames), flags&bufferFlagsSilent != 0)
			comCall(c.cc, slotBufRelease, uintptr(frames))
		}
		c.mu.Unlock()
		if len(out) >= c.f.Rate*c.f.Channels/50 { // 20 ms
			return out, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// convert appends frames of the mix format as 16-bit PCM, the first two
// channels.
func (c *wasapiCapture) convert(out []int16, data unsafe.Pointer, frames int, silent bool) []int16 {
	oc := c.f.Channels
	if silent || data == nil {
		return append(out, make([]int16, frames*oc)...)
	}
	switch {
	case c.float:
		src := unsafe.Slice((*float32)(data), frames*c.inCh)
		for i := range frames {
			for ch := range oc {
				v := src[i*c.inCh+ch]
				out = append(out, int16(math.Max(-1, math.Min(1, float64(v)))*32767))
			}
		}
	case c.mix.Bits == 32:
		src := unsafe.Slice((*int32)(data), frames*c.inCh)
		for i := range frames {
			for ch := range oc {
				out = append(out, int16(src[i*c.inCh+ch]>>16))
			}
		}
	default:
		src := unsafe.Slice((*int16)(data), frames*c.inCh)
		for i := range frames {
			out = append(out, src[i*c.inCh:i*c.inCh+oc]...)
		}
	}
	return out
}

func (c *wasapiCapture) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	comCall(c.ac, slotACStop)
	release(c.cc)
	release(c.ac)
}

type wasapiPlayer struct {
	ac, rc  *comObj
	f       AudioFormat
	bufSize uint32 // frames
	mu      sync.Mutex
	closed  bool
}

// Playback latency: what Write keeps queued at most, and the silence it
// puts first after running dry (some room for the network's jitter).
const (
	playMax     = 150 * time.Millisecond
	playCushion = 40 * time.Millisecond
)

func openAudioPlayer(f AudioFormat) (AudioPlayer, error) {
	if f.Rate <= 0 || f.Channels < 1 || f.Channels > 2 {
		return nil, fmt.Errorf("audio format %+v", f)
	}
	ac, err := audioClient()
	if err != nil {
		return nil, err
	}
	p := &wasapiPlayer{ac: ac, f: f}
	wf := waveFormat{Tag: waveFormatPCM, Channels: uint16(f.Channels), Rate: uint32(f.Rate), Bits: 16,
		BlockAlign: uint16(2 * f.Channels), AvgBytes: uint32(2 * f.Channels * f.Rate)}
	if err := hr("IAudioClient::Initialize(render)", comCall(ac, slotACInitialize, 0, streamAutoConvert, hns/5, 0, uintptr(unsafe.Pointer(&wf)), 0)); err != nil {
		release(ac)
		return nil, err
	}
	if err := hr("GetBufferSize", comCall(ac, slotACGetBufferSize, uintptr(unsafe.Pointer(&p.bufSize)))); err != nil {
		release(ac)
		return nil, err
	}
	if err := hr("GetService(IAudioRenderClient)", comCall(ac, slotACGetService, uintptr(unsafe.Pointer(&iidIAudioRenderClient)), uintptr(unsafe.Pointer(&p.rc)))); err != nil {
		release(ac)
		return nil, err
	}
	if err := hr("IAudioClient::Start", comCall(ac, slotACStart)); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func (p *wasapiPlayer) Write(pcm []int16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("audio player closed")
	}
	ch := p.f.Channels
	var padding uint32
	if err := hr("GetCurrentPadding", comCall(p.ac, slotACGetCurrentPadding, uintptr(unsafe.Pointer(&padding)))); err != nil {
		return err
	}
	if padding == 0 { // ran dry (or the start): a cushion against jitter
		pcm = append(make([]int16, p.f.Rate*ch*int(playCushion/time.Millisecond)/1000), pcm...)
	}
	limit := min(uint32(p.f.Rate*int(playMax/time.Millisecond)/1000), p.bufSize)
	if padding >= limit {
		return nil // far behind: drop, catch up
	}
	frames := min(uint32(len(pcm)/ch), limit-padding)
	if frames == 0 {
		return nil
	}
	var buf unsafe.Pointer
	if err := hr("IAudioRenderClient::GetBuffer", comCall(p.rc, slotBufGet, uintptr(frames), uintptr(unsafe.Pointer(&buf)))); err != nil {
		return err
	}
	copy(unsafe.Slice((*int16)(buf), int(frames)*ch), pcm)
	comCall(p.rc, slotBufRelease, uintptr(frames), 0)
	return nil
}

func (p *wasapiPlayer) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	comCall(p.ac, slotACStop)
	release(p.rc)
	release(p.ac)
}
