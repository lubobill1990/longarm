//go:build !windows

package screen

func openAudioCapture() (AudioCapture, error)          { return nil, ErrUnsupported }
func openAudioPlayer(AudioFormat) (AudioPlayer, error) { return nil, ErrUnsupported }
