//go:build linux && noopenh264

package screen

// Built without OpenH264 (a static binary): JPEG tiles only.

func newOpenH264Encoder(EncoderOptions) (VideoEncoder, error) { return nil, ErrUnsupported }
func newOpenH264Decoder(int, int) (VideoDecoder, error)       { return nil, ErrUnsupported }

// OpenH264Version says which OpenH264 H.264 uses ("" until loaded).
func OpenH264Version() string { return "" }
