package notices

import (
	"bytes"
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	for _, want := range []string{
		"Longarm itself is licensed under the MIT License",
		"Go modules (longarm, longarmd",
		"Web UI packages",
		"longarm-hub container image: Litestream",
		"Go (standard library and runtime)",
		"github.com/spf13/cobra",
		"SIL OPEN FONT LICENSE",
	} {
		if !strings.Contains(Text, want) {
			t.Errorf("THIRD_PARTY_NOTICES.txt lacks %q", want)
		}
	}
}

func TestCmd(t *testing.T) {
	c := Cmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetArgs(nil)
	if err := c.Execute(); err != nil || out.String() != Text {
		t.Fatalf("licenses: %v, %d bytes, want %d", err, out.Len(), len(Text))
	}
}
