// Package audiodrv installs and removes the Longarm audio driver
// (drivers/windows/audio): a virtual speaker, "Longarm Microphone Feed",
// whose sound comes out of a virtual microphone, "Longarm Microphone". The
// remote desktop plays the viewer's microphone into the speaker; programs on
// the worker record the microphone (docs/remote-desktop.md "Microphone").
//
// The driver is a kernel driver: Windows loads it with Secure Boot on only
// once Microsoft signed it; until then on test-signing machines only. A
// worker without it offers no microphone (rdesk).
package audiodrv

import "errors"

// HardwareID is the driver's root-enumerated device (LongarmAudio.inf).
const HardwareID = `ROOT\LongarmVirtualAudio`

// ErrUnsupported: not Windows.
var ErrUnsupported = errors.New("the Longarm audio driver is for Windows")

// Install adds the driver package inf (LongarmAudio.inf, beside its .sys and
// .cat) and creates its device, or updates the driver of the device there
// is (elevated). reboot: Windows wants a restart to finish.
func Install(inf string) (reboot bool, err error) { return install(inf) }

// Uninstall removes the device and the driver package (elevated).
func Uninstall() error { return uninstall() }

// Devices are the driver's devices here and their state ("" when none).
func Devices() ([]string, error) { return devices() }
