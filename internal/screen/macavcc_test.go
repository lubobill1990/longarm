package screen

import (
	"bytes"
	"testing"
)

func TestAnnexBNALs(t *testing.T) {
	sps, pps, idr := []byte{0x67, 0x64, 0x00, 0x33}, []byte{0x68, 0xEE, 0x3C, 0x80}, []byte{0x65, 0x88, 0x00, 0x01, 0x00, 0x00, 0x02}
	// 4- and 3-byte start codes, a trailing zero byte.
	au := append([]byte{0, 0, 0, 1}, sps...)
	au = append(append(au, 0, 0, 1), pps...)
	au = append(append(au, 0, 0, 0, 1), idr...)
	got := annexBNALs(au)
	want := [][]byte{sps, pps, idr}
	if len(got) != len(want) {
		t.Fatalf("got %d NAL units, want %d: %x", len(got), len(want), got)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("NAL %d: %x, want %x", i, got[i], want[i])
		}
	}
	if n := annexBNALs(nil); len(n) != 0 {
		t.Errorf("empty: %x", n)
	}
	if n := annexBNALs([]byte{0, 0, 1}); len(n) != 0 {
		t.Errorf("only a start code: %x", n)
	}

	avcc := appendAVCC(nil, got...)
	if !bytes.Equal(avcc[:4], []byte{0, 0, 0, 4}) || len(avcc) != 4*3+len(sps)+len(pps)+len(idr) {
		t.Fatalf("AVCC: %x", avcc)
	}
	back, ok := appendAnnexB(nil, avcc, 4)
	if !ok {
		t.Fatal("appendAnnexB failed")
	}
	again := annexBNALs(back)
	for i := range want {
		if !bytes.Equal(again[i], want[i]) {
			t.Errorf("round trip NAL %d: %x, want %x", i, again[i], want[i])
		}
	}
	// 2-byte lengths.
	if b, ok := appendAnnexB(nil, []byte{0, 2, 0x41, 0x9A}, 2); !ok || !bytes.Equal(b, []byte{0, 0, 0, 1, 0x41, 0x9A}) {
		t.Errorf("2-byte lengths: %x %v", b, ok)
	}
	if _, ok := appendAnnexB(nil, []byte{0, 0, 0, 9, 1}, 4); ok {
		t.Error("a length past the end was taken")
	}
}
