//go:build linux && !noopenh264

package screen

import (
	"os"
	"testing"
	"unsafe"
)

// The structures as codec_app_def.h/codec_def.h lay them out (LP64).
func TestOpenH264Layout(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want uintptr
	}{
		{"SEncParamBase", unsafe.Sizeof(encParamBase{}), 24},
		{"SSourcePicture.pData", unsafe.Offsetof(sourcePicture{}.data), 24},
		{"SSourcePicture.uiTimeStamp", unsafe.Offsetof(sourcePicture{}.timestamp), 64},
		{"SSourcePicture", unsafe.Sizeof(sourcePicture{}), 80},
		{"SLayerBSInfo.iNalCount", unsafe.Offsetof(layerBSInfo{}.nalCount), 16},
		{"SLayerBSInfo.pNalLengthInByte", unsafe.Offsetof(layerBSInfo{}.nalLength), 24},
		{"SLayerBSInfo.pBsBuf", unsafe.Offsetof(layerBSInfo{}.bs), 32},
		{"SLayerBSInfo", unsafe.Sizeof(layerBSInfo{}), 56},
		{"SFrameBSInfo.sLayerInfo", unsafe.Offsetof(frameBSInfo{}.layers), 8},
		{"SDecodingParam.eEcActiveIdc", unsafe.Offsetof(decodingParam{}.ecActiveIdc), 16},
		{"SDecodingParam.sVideoProperty", unsafe.Offsetof(decodingParam{}.propSize), 24},
		{"SDecodingParam", unsafe.Sizeof(decodingParam{}), 32},
		{"SBufferInfo.UsrData", unsafe.Offsetof(bufferInfo{}.width), 24},
		{"SBufferInfo.iStride", unsafe.Offsetof(bufferInfo{}.stride), 36},
		{"SBufferInfo.pDst", unsafe.Offsetof(bufferInfo{}.dst), 48},
		{"SBufferInfo", unsafe.Sizeof(bufferInfo{}), 72},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d, want %d", c.name, c.got, c.want)
		}
	}
}

// An encode/decode round trip, when OpenH264 is here (installed or
// cached; LONGARM_TEST_OPENH264_DOWNLOAD=1 fetches it).
func TestOpenH264RoundTrip(t *testing.T) {
	if openH264Verified() == "" && os.Getenv("LONGARM_OPENH264") == "" && os.Getenv("LONGARM_TEST_OPENH264_DOWNLOAD") == "" {
		if err := openH264.open("libopenh264.so.8"); err != nil {
			t.Skip("no OpenH264 here")
		}
	}
	const w, h = 320, 240
	enc, err := NewH264Encoder(EncoderOptions{Width: w, Height: h, FPS: 30, Bitrate: 2_000_000})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	t.Logf("OpenH264 %s", OpenH264Version())
	dec, err := NewH264Decoder(w, h)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	frame := make([]byte, w*h*4)
	colours := [4][3]byte{{255, 0, 0}, {0, 255, 0}, {0, 0, 255}, {255, 255, 255}} // RGB per quadrant
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := colours[(y/(h/2))*2+x/(w/2)]
			o := (y*w + x) * 4
			frame[o], frame[o+1], frame[o+2], frame[o+3] = c[2], c[1], c[0], 255
		}
	}
	var img []byte
	for i := 0; i < 3 && img == nil; i++ {
		au, err := enc.Encode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && !isIDRAU(au) {
			t.Error("the first access unit is not an IDR")
		}
		var gw, gh int
		if img, gw, gh, err = dec.Decode(au); err != nil {
			t.Fatal(err)
		}
		if img != nil && (gw != w || gh != h) {
			t.Fatalf("decoded %dx%d", gw, gh)
		}
	}
	if img == nil {
		t.Fatal("no picture decoded")
	}
	for q, c := range colours {
		x, y := w/4+(q%2)*w/2, h/4+(q/2)*h/2
		p := img[(y*w+x)*4:]
		for i, want := range []byte{c[2], c[1], c[0]} {
			if d := int(p[i]) - int(want); d > 12 || d < -12 {
				t.Errorf("quadrant %d: BGR %v, want about %v", q, p[:3], []byte{c[2], c[1], c[0]})
				break
			}
		}
	}
	// A key frame on request.
	enc.ForceKeyframe()
	au, err := enc.Encode(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !isIDRAU(au) {
		t.Error("ForceKeyframe: no IDR")
	}
}

func isIDRAU(au []byte) bool {
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 && au[i+3]&0x1F == 5 {
			return true
		}
	}
	return false
}
