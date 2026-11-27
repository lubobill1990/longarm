package main

import (
	"fmt"
	"os"
	"sync"

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

// micSender sends this machine's microphone to the worker while the worker
// says the microphone option is on (DesktopState.options.microphone).
type micSender struct {
	mu     sync.Mutex
	c      screen.AudioCapture // capturing
	send   func(*pb.DesktopClientMessage) error
	failed bool
}

func (m *micSender) set(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case on && m.c == nil:
		c, err := screen.OpenMicCapture()
		if err != nil {
			if !m.failed {
				m.failed = true
				fmt.Fprintf(os.Stderr, "longarm: your microphone: %v\n", err)
			}
			return
		}
		m.c = c
		go func() {
			f := c.Format()
			enc := screen.NewADPCMEncoder(f.Channels)
			var seq uint64
			for {
				pcm, err := c.Read()
				if err != nil {
					return // closed
				}
				seq++
				if m.send(&pb.DesktopClientMessage{Msg: &pb.DesktopClientMessage_Microphone{Microphone: &pb.DesktopAudio{
					Codec: screen.AudioCodec, Rate: int32(f.Rate), Channels: int32(f.Channels), Data: enc.Encode(pcm), Seq: seq}}}) != nil {
					return
				}
			}
		}()
	case !on && m.c != nil:
		m.c.Close()
		m.c = nil
	}
}

func (s *soundPlayer) close() {
	if s.p != nil {
		s.p.Close()
		s.p = nil
	}
}
