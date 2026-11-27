package screen

import (
	"math"
	"testing"
)

// TestADPCM: a tone survives encoding (close to the original), packets
// decode on their own, and odd frame counts and mono work.
func TestADPCM(t *testing.T) {
	for _, ch := range []int{1, 2} {
		enc := NewADPCMEncoder(ch)
		for packet := range 5 {
			frames := 961 // 20 ms at 48 kHz, odd on purpose
			pcm := make([]int16, frames*ch)
			for i := range frames {
				v := int16(12000 * math.Sin(2*math.Pi*440*float64(packet*frames+i)/48000))
				for c := range ch {
					pcm[i*ch+c] = v / int16(c+1)
				}
			}
			data := enc.Encode(pcm)
			got, err := DecodeADPCM(data, ch)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(pcm) {
				t.Fatalf("%d channels: %d samples back, %d sent", ch, len(got), len(pcm))
			}
			var errSum, sigSum float64
			for i := range pcm {
				d := float64(got[i]) - float64(pcm[i])
				errSum += d * d
				sigSum += float64(pcm[i]) * float64(pcm[i])
			}
			// The first packet starts at the smallest step: it may adapt.
			if snr := 10 * math.Log10(sigSum/errSum); snr < 20 || packet > 0 && snr < 30 {
				t.Errorf("%d channels, packet %d: SNR %.1f dB", ch, packet, snr)
			}
		}
	}
	if _, err := DecodeADPCM([]byte{1, 2}, 2); err == nil {
		t.Error("a short packet decoded")
	}
}
