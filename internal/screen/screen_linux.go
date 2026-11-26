//go:build linux

package screen

// Linux: X11 (x11_linux.go, capture_linux.go, input_linux.go).

func newH264Encoder(EncoderOptions) (VideoEncoder, error) { return nil, ErrUnsupported }
func newH264Decoder(int, int) (VideoDecoder, error)       { return nil, ErrUnsupported }
func runViewer(ViewerOptions) error                       { return ErrUnsupported }

const canView = false
