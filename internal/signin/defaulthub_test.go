package signin

import (
	"testing"

	"github.com/lubobill1990/longarm/internal/version"
)

// TestFallbackHubOrder: the installer's hub wins over the build's default;
// the build's CA pin applies to the installer's hub only when it is the
// same hub; nothing at all = "" (the caller asks for a hub).
func TestFallbackHubOrder(t *testing.T) {
	none := func() (string, string) { return "", "" }
	inst := func(h, fp string) func() (string, string) { return func() (string, string) { return h, fp } }
	builtIn := inst("longarm.example.com", "ab12")
	for _, c := range []struct {
		name               string
		installed, builtIn func() (string, string)
		hub, fp, from      string
	}{
		{"nothing", none, none, "", "", ""},
		{"built-in only", none, builtIn, "longarm.example.com", "ab12", HubFromBuild},
		{"installer wins", inst("other.example.com", ""), builtIn, "other.example.com", "", HubFromInstaller},
		{"installer pin wins", inst("longarm.example.com", "cd34"), builtIn, "longarm.example.com", "cd34", HubFromInstaller},
		{"installer = built-in: build's pin", inst("https://LONGARM.example.com/", ""), builtIn, "https://LONGARM.example.com/", "ab12", HubFromInstaller},
		{"installer = built-in with port", inst("longarm.example.com:7443", ""), builtIn, "longarm.example.com:7443", "ab12", HubFromInstaller},
		{"installer only", inst(" x.example.com ", "ff"), none, "x.example.com", "ff", HubFromInstaller},
	} {
		hub, fp, from := fallbackHub(c.installed, c.builtIn)
		if hub != c.hub || fp != c.fp || from != c.from {
			t.Errorf("%s: got %q %q %q, want %q %q %q", c.name, hub, fp, from, c.hub, c.fp, c.from)
		}
	}
}

func TestDefaultHubFromBuild(t *testing.T) {
	oh, oc := version.DefaultHub, version.DefaultHubCA
	t.Cleanup(func() { version.DefaultHub, version.DefaultHubCA = oh, oc })
	version.DefaultHub, version.DefaultHubCA = "", "AB:CD"
	if h, fp := DefaultHub(); h != "" || fp != "" {
		t.Errorf("no default hub: got %q %q", h, fp)
	}
	version.DefaultHub = " longarm.example.com "
	h, fp := DefaultHub()
	if h != "longarm.example.com" || fp == "" {
		t.Errorf("got %q %q", h, fp)
	}
}
