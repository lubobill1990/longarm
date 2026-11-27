package screen

import (
	"encoding/binary"
	"errors"
)

// Sound for the remote desktop: the worker's speakers played on the
// viewer's (the viewer's microphone on the worker is to come,
// todo/remote-desktop.md). 16-bit PCM, interleaved, at most two channels;
// on the wire IMA ADPCM (4 bits a sample: 384 kbit/s for 48 kHz stereo).

// AudioFormat is 16-bit PCM's rate and channels.
type AudioFormat struct{ Rate, Channels int }

// AudioCapture is what this machine's speakers play.
type AudioCapture interface {
	Format() AudioFormat
	// Read waits for the next samples (some tens of milliseconds; nothing
	// while nothing plays).
	Read() ([]int16, error)
	Close()
}

// OpenAudioCapture captures what the default speakers play.
func OpenAudioCapture() (AudioCapture, error) { return openAudioCapture() }

// AudioPlayer plays samples on this machine's speakers.
type AudioPlayer interface {
	// Write queues samples; what does not fit in a short buffer is
	// dropped (sound late is worse than a gap).
	Write([]int16) error
	Close()
}

// OpenAudioPlayer opens the default speakers for f.
func OpenAudioPlayer(f AudioFormat) (AudioPlayer, error) { return openAudioPlayer(f) }

// AudioCodec is the codec name on the wire (DesktopAudio.codec).
const AudioCodec = "ima-adpcm"

// IMA ADPCM. A packet stands alone (a lost one is a gap, not noise): per
// channel a header of the first sample (int16, little endian) and the step
// index (a byte, then a zero byte), then for every further frame a 4-bit
// code per channel, low nibble first.

var imaIndex = [16]int{-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8}

var imaStep = [89]int{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230,
	253, 279, 307, 337, 371, 408, 449, 494, 544, 598, 658, 724, 796, 876, 963,
	1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024, 3327,
	3660, 4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493, 10442,
	11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794,
	32767,
}

type imaState struct{ pred, index int }

func (s *imaState) decode(code byte) int16 {
	step := imaStep[s.index]
	diff := step >> 3
	if code&1 != 0 {
		diff += step >> 2
	}
	if code&2 != 0 {
		diff += step >> 1
	}
	if code&4 != 0 {
		diff += step
	}
	if code&8 != 0 {
		s.pred -= diff
	} else {
		s.pred += diff
	}
	s.pred = min(max(s.pred, -32768), 32767)
	s.index = min(max(s.index+imaIndex[code], 0), 88)
	return int16(s.pred)
}

func (s *imaState) encode(x int16) byte {
	diff := int(x) - s.pred
	var code byte
	if diff < 0 {
		code, diff = 8, -diff
	}
	step := imaStep[s.index]
	if diff >= step {
		code |= 4
		diff -= step
	}
	if diff >= step>>1 {
		code |= 2
		diff -= step >> 1
	}
	if diff >= step>>2 {
		code |= 1
	}
	s.decode(code) // the encoder follows the decoder's state
	return code
}

// ADPCMEncoder encodes a stream's packets; it keeps the step sizes from
// one packet to the next (the header carries them), so a packet starts
// adapted to the sound.
type ADPCMEncoder struct {
	ch int
	st []imaState
}

func NewADPCMEncoder(channels int) *ADPCMEncoder {
	return &ADPCMEncoder{ch: channels, st: make([]imaState, channels)}
}

// Encode encodes interleaved samples (whole frames).
func (e *ADPCMEncoder) Encode(pcm []int16) []byte {
	ch, st := e.ch, e.st
	frames := len(pcm) / ch
	if frames == 0 {
		return nil
	}
	out := make([]byte, 4*ch, 4*ch+((frames-1)*ch+1)/2)
	for c := range ch {
		st[c].pred = int(pcm[c])
		binary.LittleEndian.PutUint16(out[4*c:], uint16(pcm[c]))
		out[4*c+2] = byte(st[c].index)
	}
	var cur byte
	n := 0
	for i := ch; i < frames*ch; i++ {
		code := st[i%ch].encode(pcm[i])
		if n%2 == 0 {
			cur = code
		} else {
			out = append(out, cur|code<<4)
		}
		n++
	}
	if n%2 == 1 {
		out = append(out, cur)
	}
	return out
}

// DecodeADPCM decodes a packet of EncodeADPCM.
func DecodeADPCM(data []byte, ch int) ([]int16, error) {
	if ch < 1 || len(data) < 4*ch {
		return nil, errors.New("adpcm: short packet")
	}
	st := make([]imaState, ch)
	out := make([]int16, 0, ch+2*(len(data)-4*ch))
	for c := range ch {
		st[c].pred = int(int16(binary.LittleEndian.Uint16(data[4*c:])))
		st[c].index = min(int(data[4*c+2]), 88)
		out = append(out, int16(st[c].pred))
	}
	codes := data[4*ch:]
	total := 2 * len(codes)
	// An odd count leaves a padding nibble: whole frames only.
	total -= total % ch
	for i := range total {
		b := codes[i/2]
		if i%2 == 1 {
			b >>= 4
		}
		out = append(out, st[(ch+i)%ch].decode(b&0xF))
	}
	return out, nil
}
