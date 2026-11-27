package main

import (
	"fmt"
	"os"

	"longarm/internal/screen"
	pb "longarm/proto/longarm/v1"
)

// soundPlayer plays the worker's sound (DesktopAudio) on this machine's
// speakers, opening them on the first packet and again when the format
// changes.
type soundPlayer struct {
	p      screen.AudioPlayer
	f      screen.AudioFormat
	failed bool // no speakers here: say so once
}

func (s *soundPlayer) play(a *pb.DesktopAudio) {
	if a.Codec != screen.AudioCodec {
		return
	}
	f := screen.AudioFormat{Rate: int(a.Rate), Channels: int(a.Channels)}
	if s.p == nil || f != s.f {
		s.close()
		p, err := screen.OpenAudioPlayer(f)
		if err != nil {
			if !s.failed {
				s.failed = true
				fmt.Fprintf(os.Stderr, "longarm: the worker's sound can't play here: %v\n", err)
			}
			return
		}
		s.p, s.f = p, f
	}
	pcm, err := screen.DecodeADPCM(a.Data, f.Channels)
	if err != nil {
		return
	}
	s.p.Write(pcm)
}

func (s *soundPlayer) close() {
	if s.p != nil {
		s.p.Close()
		s.p = nil
	}
}
