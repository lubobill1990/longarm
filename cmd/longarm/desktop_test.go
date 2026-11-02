package main

import (
	"io"
	"strings"
	"testing"
	"time"

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
		return chooseDevBox(testBoxes, st, worker, "dc1", want, pick, devbox.Facts{}, strings.NewReader(input), io.Discard, tty)
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

// TestChooseDevBoxMatched: with the worker's facts the Dev Box is picked
// without a question; --pick asks, the likeliest first and marked.
func TestChooseDevBoxMatched(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	box := func(name, created string) devbox.DevBox {
		return devbox.DevBox{Name: name, ProjectName: "ODSP", CreatedTime: at(created)}
	}
	boxes := []devbox.DevBox{box("bot1", "2026-07-09T03:03:57Z"), box("arm3", "2026-10-27T06:43:42Z"), box("arm4", "2026-10-30T14:19:10Z")}
	st := &desktopStore{Workers: map[string]desktopLink{}}
	facts := devbox.Facts{Installed: at("2026-10-27T06:51:55Z")}
	var out strings.Builder
	b, err := chooseDevBox(boxes, st, "cpc-alice-252m5", "dc1", "", false, facts, strings.NewReader(""), &out, false)
	if err != nil || b.Name != "arm3" || !strings.Contains(out.String(), "created 8m0s before Windows was installed on it") {
		t.Fatalf("matched: %+v %v %q", b, err, out.String())
	}
	out.Reset()
	b, err = chooseDevBox(boxes, st, "cpc-alice-252m5", "dc1", "", true, facts, strings.NewReader("1\n"), &out, true)
	if err != nil || b.Name != "arm3" || !strings.Contains(out.String(), "1) ODSP/arm3  -, ? (likely)") {
		t.Fatalf("--pick: %+v %v %q", b, err, out.String())
	}
}
