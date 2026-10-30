package main

import (
	"io"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/devbox"
)

var testBoxes = []devbox.DevBox{
	{Name: "arm1", ProjectName: "ODSP", PowerState: "Running"},
	{Name: "arm2", ProjectName: "ODSP", PowerState: "Running"},
	{Name: "arm1", ProjectName: "Other", PowerState: "Stopped"},
}

func TestChooseDevBox(t *testing.T) {
	st := &desktopStore{Workers: map[string]desktopLink{
		"w-a": {DevCenter: "dc1", Project: "ODSP", DevBox: "arm1", URL: "ms-avd:connect?a"},
		"w-b": {DevCenter: "dc1", Project: "Other", DevBox: "arm1", URL: "ms-avd:connect?b"},
	}}
	pick := func(worker, want string, pick bool, input string, tty bool) (devbox.DevBox, error) {
		return chooseDevBox(testBoxes, st, worker, "dc1", want, pick, strings.NewReader(input), io.Discard, tty)
	}
	// The only Dev Box no other worker has.
	if b, err := pick("w-c", "", false, "", false); err != nil || b.Name != "arm2" {
		t.Fatalf("only free: %+v, %v", b, err)
	}
	// --pick asks even then; without a terminal it can't.
	if _, err := pick("w-c", "", true, "", false); err == nil || !strings.Contains(err.Error(), "--devbox") {
		t.Fatalf("no tty: %v", err)
	}
	if b, err := pick("w-c", "", true, "3\n", true); err != nil || b.ProjectName != "Other" {
		t.Fatalf("chosen: %+v, %v", b, err)
	}
	for _, in := range []string{"0\n", "4\n", "x\n", ""} {
		if _, err := pick("w-c", "", true, in, true); err == nil {
			t.Errorf("input %q accepted", in)
		}
	}
	// --devbox: by name, ambiguous names need the project.
	if b, err := pick("w-c", "ARM2", false, "", false); err != nil || b.Name != "arm2" {
		t.Fatalf("by name: %+v, %v", b, err)
	}
	if _, err := pick("w-c", "arm1", false, "", false); err == nil || !strings.Contains(err.Error(), "several projects") {
		t.Fatalf("ambiguous: %v", err)
	}
	if b, err := pick("w-c", "other/arm1", false, "", false); err != nil || b.ProjectName != "Other" {
		t.Fatalf("project/name: %+v, %v", b, err)
	}
	if _, err := pick("w-c", "nope", false, "", false); err == nil || !strings.Contains(err.Error(), "ODSP/arm2") {
		t.Fatalf("unknown: %v", err)
	}
	// A worker's own Dev Box does not count as taken.
	st.Workers["w-c"] = desktopLink{DevCenter: "dc1", Project: "ODSP", DevBox: "arm2"}
	if b, err := pick("w-c", "", false, "", false); err != nil || b.Name != "arm2" {
		t.Fatalf("own: %+v, %v", b, err)
	}
}

func TestRDPFile(t *testing.T) {
	c := rdpFileContent("127.0.0.1:53389")
	if !strings.HasPrefix(c, "full address:s:127.0.0.1:53389\r\n") {
		t.Fatalf("got %q", c)
	}
	if got := sanitizeFileName(`w1/..\x y`); got != "w1____x_y" {
		t.Fatalf("got %q", got)
	}
}

func TestDesktopStore(t *testing.T) {
	old := configDir
	defer func() { configDir = old }()
	configDir = t.TempDir()
	st := loadDesktopStore()
	st.Workers["w1"] = desktopLink{DevBox: "arm1", URL: "ms-avd:connect?x"}
	st.Endpoints["t/dc"] = "https://t-dc.westus3.devcenter.azure.com"
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	got := loadDesktopStore()
	if got.Workers["w1"].DevBox != "arm1" || got.Endpoints["t/dc"] == "" {
		t.Fatalf("got %+v", got)
	}
}
