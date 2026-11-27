//go:build windows

package screen

import (
	"math"
	"os"
	"testing"
	"time"
)

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
