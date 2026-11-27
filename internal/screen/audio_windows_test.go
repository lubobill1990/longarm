//go:build windows

package screen

import (
	"math"
	"os"
	"testing"
	"time"
	"unsafe"
)

// TestAudioDeviceNames: playback devices are found by their friendly names
// (how the worker finds the Longarm audio driver's microphone feed), and a
// name nothing has is not.
func TestAudioDeviceNames(t *testing.T) {
	procCoInitializeEx.Call(0, 0)
	var en *comObj
	if err := hr("CoCreateInstance", callProc(procCoCreateInstance, uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&en)))); err != nil {
		t.Skip(err)
	}
	defer release(en)
	var dev *comObj
	if comCall(en, slotEnumGetDefaultEndpoint, eRender, eConsole, uintptr(unsafe.Pointer(&dev))) != 0 {
		t.Skip("no speakers here")
	}
	name := deviceName(dev)
	release(dev)
	if name == "" {
		t.Fatal("the default speakers have no friendly name")
	}
	t.Logf("default speakers: %q", name)
	if !audioOutputExists(name) {
		t.Errorf("%q not found by its name", name)
	}
	if audioOutputExists("no such device " + name) {
		t.Error("a device that isn't there was found")
	}
	t.Logf("the Longarm audio driver here: %v", MicFeedAvailable())
}

// TestAudioLoopback plays a quiet tone and hears it in the loopback
// capture: the player and the capture both work. Machines without
// speakers skip; it makes a sound, so only with LONGARM_AUDIO_TEST=1.
func TestAudioLoopback(t *testing.T) {
	if os.Getenv("LONGARM_AUDIO_TEST") == "" {
		t.Skip("plays a sound: LONGARM_AUDIO_TEST=1")
	}
	capt, err := OpenAudioCapture()
	if err != nil {
		t.Skipf("no loopback capture here: %v", err)
	}
	defer capt.Close()
	f := capt.Format()
	t.Logf("capture %+v", f)
	pl, err := OpenAudioPlayer(AudioFormat{Rate: 48000, Channels: 2})
	if err != nil {
		t.Fatalf("player: %v", err)
	}
	defer pl.Close()
	done := make(chan struct{})
	go func() {
		n := 0
		for {
			select {
			case <-done:
				return
			default:
			}
			pcm := make([]int16, 960*2) // 20 ms
			for i := range 960 {
				v := int16(3000 * math.Sin(2*math.Pi*440*float64(n+i)/48000))
				pcm[2*i], pcm[2*i+1] = v, v
			}
			n += 960
			pl.Write(pcm)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	defer close(done)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pcm, err := capt.Read()
		if err != nil {
			t.Fatal(err)
		}
		peak := 0
		for _, s := range pcm {
			peak = max(peak, int(s), -int(s))
		}
		if peak > 500 {
			t.Logf("heard the tone: peak %d in %d samples", peak, len(pcm))
			return
		}
	}
	t.Fatal("the loopback capture never heard the tone")
}
