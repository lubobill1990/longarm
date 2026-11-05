package updatesvc

import (
	"net/url"
	"testing"
)

// TestRedirectAllowed: HTTPS to the hub's host or GitHub's release
// downloads only (a hub without releases of its own redirects to GitHub).
func TestRedirectAllowed(t *testing.T) {
	hub, _ := url.Parse("https://longarm.example.com")
	for to, want := range map[string]bool{
		"https://LONGARM.example.com/releases/1.2.0/x.msi":                          true,
		"https://github.com/o/r/releases/download/v1.2.0/longarm-windows-amd64.msi": true,
		"https://objects.githubusercontent.com/x":                                   true,
		"https://release-assets.githubusercontent.com/x":                            true,
		"http://github.com/o/r/releases/download/v1.2.0/x.msi":                      false,
		"http://longarm.example.com/x":                                              false,
		"https://evil.example.com/x":                                                false,
		"https://github.com.evil.example/x":                                         false,
	} {
		u, _ := url.Parse(to)
		if got := redirectAllowed(u, hub); got != want {
			t.Errorf("%s: %v, want %v", to, got, want)
		}
	}
	u, _ := url.Parse("https://github.com/x")
	if redirectAllowed(u, nil) {
		t.Error("no hub URL: allowed")
	}
}
