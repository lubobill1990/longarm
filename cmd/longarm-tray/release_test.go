package main

import (
	"os"
	"strings"
	"testing"
)

// TestReleaseTrayIsProduction: every release-like build of the tray uses
// the "production" tag (Wails: no devtools, no WebView2 debug context
// menu); development builds keep them.
func TestReleaseTrayIsProduction(t *testing.T) {
	for file, want := range map[string]string{
		"../../.goreleaser.yaml":                     "tags: [production]",
		"../../packaging/linux/goreleaser-tray.yaml": "-tags=traynative,production",
		"../../.github/workflows/ci.yml":             `go build -trimpath -tags production -ldflags "-H windowsgui $hub" -o "$bin\longarm-tray.exe"`,
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s: no %q", file, want)
		}
	}
}
