package attach

import (
	"bytes"
	"io"
	"net"
	"testing"
)

func TestFrames(t *testing.T) {
	a, b := net.Pipe()
	ca, cb := NewConn(a), NewConn(b)
	go func() {
		ca.WriteFrame(Hello, []byte{FlagReplay})
		ca.WriteFrame(Resize, ResizePayload(120, 40))
		ca.WriteFrame(Input, bytes.Repeat([]byte("x"), 100000))
		ca.WriteFrame(Exit, ExitPayload(-3))
		ca.Close()
	}()
	typ, p, err := cb.ReadFrame()
	if err != nil || typ != Hello || p[0] != FlagReplay {
		t.Fatal(typ, p, err)
	}
	typ, p, _ = cb.ReadFrame()
	if c, r, ok := ParseResize(p); typ != Resize || !ok || c != 120 || r != 40 {
		t.Fatal(typ, c, r, ok)
	}
	if typ, p, _ = cb.ReadFrame(); typ != Input || len(p) != 100000 {
		t.Fatal(typ, len(p))
	}
	if typ, p, _ = cb.ReadFrame(); typ != Exit || ParseExit(p) != -3 {
		t.Fatal(typ, ParseExit(p))
	}
	if _, _, err := cb.ReadFrame(); err != io.EOF {
		t.Fatalf("after close: %v", err)
	}
	if err := ca.WriteFrame(Input, make([]byte, MaxFrame+1)); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestSendPayloadAndInfo(t *testing.T) {
	p := SendPayload(1500, SourceSend, []byte("hi\r"))
	idle, src, data, err := ParseSend(p)
	if err != nil || idle != 1500 || src != SourceSend || string(data) != "hi\r" {
		t.Fatal(idle, src, data, err)
	}
	if _, _, _, err := ParseSend([]byte{0, 0, 0, 1, 9, 'x'}); err == nil {
		t.Fatal("short source accepted")
	}
	if _, _, _, err := ParseSend(nil); err == nil {
		t.Fatal("empty frame accepted")
	}
	st, err := DecodeInfo(EncodeInfo(InfoState{InputSeq: 7, LastInputSource: SourceAttach, Sent: true}))
	if err != nil || st.InputSeq != 7 || st.LastInputSource != SourceAttach || !st.Sent || !st.LastInput.IsZero() {
		t.Fatal(st, err)
	}
}

func TestSendXPayload(t *testing.T) {
	seq := uint64(0)
	p := SendXPayload(SendOpts{Source: SourceSend, IfInputSeq: &seq, AfterMs: 100}, []byte("\x1b[200~hi\x1b[201~"), []byte("\r"))
	o, data, after, err := ParseSendX(p)
	if err != nil || o.Source != SourceSend || o.IfInputSeq == nil || *o.IfInputSeq != 0 || o.AfterMs != 100 ||
		string(data) != "\x1b[200~hi\x1b[201~" || string(after) != "\r" {
		t.Fatal(o, data, after, err)
	}
	if o, data, after, err = ParseSendX(SendXPayload(SendOpts{Source: SourceSend}, []byte("x"), nil)); err != nil || o.IfInputSeq != nil || string(data) != "x" || len(after) != 0 {
		t.Fatal(o, data, after, err)
	}
	for _, bad := range [][]byte{nil, {0, 0, 0, 9, '{'}, append([]byte{0, 0, 0, 11}, `{"after":5}x`...)} {
		if _, _, _, err := ParseSendX(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if st := (InfoState{Features: []string{FeatureSendX}}); !st.Has(FeatureSendX) || st.Has(FeatureReplies) {
		t.Fatal(st)
	}
}
