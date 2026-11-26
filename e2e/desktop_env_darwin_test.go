//go:build darwin

package e2e

import (
	"strings"
	"testing"

	"github.com/ebitengine/purego"
)

// desktopTestEnv: macOS tests on the signed-in user's own screen, which
// needs two permissions only a person (or an MDM profile) can grant:
// Screen Recording and Accessibility. They belong to the app that started
// this test (Terminal, the CI runner), and longarmd inherits them. Skip,
// saying which is missing, without them.
func desktopTestEnv(t *testing.T) []string {
	cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Skipf("no CoreGraphics: %v", err)
	}
	as, err := purego.Dlopen("/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Skipf("no ApplicationServices: %v", err)
	}
	var session func() uintptr
	var screenRecording, accessibility func() bool
	purego.RegisterLibFunc(&session, cg, "CGSessionCopyCurrentDictionary")
	purego.RegisterLibFunc(&screenRecording, cg, "CGPreflightScreenCaptureAccess")
	purego.RegisterLibFunc(&accessibility, as, "AXIsProcessTrusted")
	if session() == 0 {
		t.Skip("this process is not in a login session with a screen (CGSessionCopyCurrentDictionary is NULL)")
	}
	var missing []string
	if !screenRecording() {
		missing = append(missing, "Screen Recording (CGPreflightScreenCaptureAccess is false)")
	}
	if !accessibility() {
		missing = append(missing, "Accessibility (AXIsProcessTrusted is false)")
	}
	if len(missing) > 0 {
		t.Skipf("macOS has not given the app running this test %s: System Settings > Privacy & Security", strings.Join(missing, " or "))
	}
	return nil
}
