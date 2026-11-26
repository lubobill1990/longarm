//go:build linux

// Placeholder: the Linux backend replaces this file (docs/remote-desktop.md).

package screen

import "runtime"

func available() string {
	return "Longarm's remote desktop does not support " + runtime.GOOS + " workers yet"
}

func openSource(int) (Source, error)                      { return nil, ErrUnsupported }
func newInjector() (Injector, error)                      { return nil, ErrUnsupported }
func newH264Encoder(EncoderOptions) (VideoEncoder, error) { return nil, ErrUnsupported }
func newH264Decoder(int, int) (VideoDecoder, error)       { return nil, ErrUnsupported }
func runViewer(ViewerOptions) error                       { return ErrUnsupported }

// Displays lists the monitors (none where capture is not supported).
func Displays() ([]Display, error) { return nil, ErrUnsupported }

const canView = false
