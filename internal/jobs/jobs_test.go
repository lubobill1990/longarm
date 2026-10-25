package jobs

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestStateMachine(t *testing.T) {
	allowed := []struct{ from, to State }{
		{Queued, Assigned}, {Assigned, Preparing}, {Assigned, Queued},
		{Preparing, Running}, {Preparing, Failed}, {Preparing, Lost},
		{Running, Done}, {Running, Failed}, {Running, Killed}, {Running, Lost},
		{Lost, Running}, {Running, Running},
	}
	for _, c := range allowed {
		if !CanTransition(c.from, c.to) {
			t.Errorf("%s -> %s should be allowed", c.from, c.to)
		}
	}
	denied := []struct{ from, to State }{
		{Done, Running}, {Failed, Running}, {Killed, Running}, {Running, Preparing},
		{Queued, Running}, {Done, Failed}, {Killed, Done}, {Lost, Done},
	}
	for _, c := range denied {
		if CanTransition(c.from, c.to) || CheckTransition(c.from, c.to) == nil {
			t.Errorf("%s -> %s should be denied", c.from, c.to)
		}
	}
	for _, s := range AllStates {
		want := s == Done || s == Failed || s == Killed || s == Lost
		if s.Terminal() != want {
			t.Errorf("%s.Terminal() = %v", s, s.Terminal())
		}
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if State("bogus").Valid() {
		t.Error("bogus state valid")
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"01JAXYZ0000000000000000000", "job-1", "a_b"} {
		if !ValidID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range []string{"", "..", "a/b", `a\b`, "a b", strings.Repeat("x", 65), "c:", "CON", "nul", "Com1"} {
		if ValidID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}

func TestStatusRoundTripAndCompatFiles(t *testing.T) {
	d := Dir(t.TempDir())
	code := 3
	st := &Status{State: Running, CreatedAt: time.Now(), PID: 1234}
	if err := d.WriteStatus(st); err != nil {
		t.Fatal(err)
	}
	st.State, st.ExitCode = Failed, &code
	if err := d.WriteStatus(st); err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Failed || got.Seq != 2 || *got.ExitCode != 3 || got.PID != 1234 {
		t.Fatalf("got %+v", got)
	}
	for name, want := range map[string]string{StateFile: "failed", PIDFile: "1234", ExitFile: "3"} {
		b, err := os.ReadFile(d.Path(name))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q (%v), want %q", name, b, err, want)
		}
	}
}

func TestReadLogOffsets(t *testing.T) {
	d := Dir(t.TempDir())
	// missing file
	data, start, size, err := d.ReadLog(0, 10)
	if err != nil || len(data) != 0 || start != 0 || size != 0 {
		t.Fatalf("missing: %q %d %d %v", data, start, size, err)
	}
	if err := os.WriteFile(d.Path(LogFile), []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		off       int64
		max       int
		want      string
		wantStart int64
	}{
		{0, 4, "0123", 0},
		{4, 100, "456789abcdef", 4},
		{-3, 100, "def", 13},
		{-100, 2, "01", 0},
		{16, 10, "", 16},
		{20, 10, "", 20},
	}
	for _, c := range cases {
		data, start, size, err := d.ReadLog(c.off, c.max)
		if err != nil || string(data) != c.want || start != c.wantStart || size != 16 {
			t.Errorf("ReadLog(%d,%d) = %q,%d,%d,%v want %q,%d", c.off, c.max, data, start, size, err, c.want, c.wantStart)
		}
	}
	// Reading while another handle appends (as the shim does).
	f, err := os.OpenFile(d.Path(LogFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write([]byte("XYZ"))
	data, start, _, _ = d.ReadLog(16, 10)
	if string(data) != "XYZ" || start != 16 {
		t.Fatalf("after append: %q %d", data, start)
	}
}

func TestKillMarker(t *testing.T) {
	d := Dir(t.TempDir())
	if d.KillRequested() {
		t.Fatal("no marker yet")
	}
	if err := d.RequestKill("test"); err != nil {
		t.Fatal(err)
	}
	if !d.KillRequested() {
		t.Fatal("marker expected")
	}
}
