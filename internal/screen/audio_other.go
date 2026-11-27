//go:build !windows

package screen

func openAudioCapture() (AudioCapture, error)                    { return nil, ErrUnsupported }
func openMicCapture() (AudioCapture, error)                      { return nil, ErrUnsupported }
func openAudioPlayer(AudioFormat) (AudioPlayer, error)           { return nil, ErrUnsupported }
func openAudioPlayerOn(string, AudioFormat) (AudioPlayer, error) { return nil, ErrUnsupported }
func audioOutputExists(string) bool                              { return false }
