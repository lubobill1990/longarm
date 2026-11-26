//go:build linux

package screen

// Linux: X11 (x11_linux.go, capture_linux.go, input_linux.go) and Cisco's
// OpenH264 (openh264*.go).

func newH264Encoder(o EncoderOptions) (VideoEncoder, error) { return newOpenH264Encoder(o) }
func newH264Decoder(w, h int) (VideoDecoder, error)       { return newOpenH264Decoder(w, h) }
func runViewer(ViewerOptions) error                       { return ErrUnsupported }

const canView = false
