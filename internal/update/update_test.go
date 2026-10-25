package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.3", "1.2.10", -1},
		{"1.10.0", "1.9.9", 1},
		{"1.2", "1.2.0", 0},
		{"1.2.3-rc.1", "1.2.3", -1},
		{"1.2.3-rc.2", "1.2.3-rc.10", -1},
		{"1.2.3-alpha", "1.2.3-beta", -1},
		{"dev", "0.0.1", -1},
		{"0.0.1", "dev", 1},
		{"1.0.0-SNAPSHOT-abc", "1.0.0", -1},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSignVerify(t *testing.T) {
	pub, priv, err := GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "k"), priv, 0o600)
	sk, err := LoadPrivateKey(filepath.Join(dir, "k"), "")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("binary contents")
	sig := Sign(sk, data, "longarmd-linux-amd64", "1.2.3")
	if err := Verify(pub, data, sig, "longarmd-linux-amd64", "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, []byte("binary contentz"), sig, "longarmd-linux-amd64", "1.2.3"); err == nil {
		t.Fatal("tampered data verified")
	}
	if err := Verify(pub, data, sig, "longarm-linux-amd64", "1.2.3"); err == nil || !strings.Contains(err.Error(), "signature is for") {
		t.Fatalf("other file: %v", err)
	}
	if err := Verify(pub, data, sig, "longarmd-linux-amd64", "1.2.4"); err == nil {
		t.Fatal("other version verified (replay)")
	}
	pub2, _, _ := GenerateKey("")
	if err := Verify(pub2, data, sig, "longarmd-linux-amd64", "1.2.3"); err == nil {
		t.Fatal("other key verified")
	}
	// Encrypted key file.
	_, enc, _ := GenerateKey("pw")
	os.WriteFile(filepath.Join(dir, "e"), enc, 0o600)
	if _, err := LoadPrivateKey(filepath.Join(dir, "e"), ""); err == nil {
		t.Fatal("encrypted key loaded without password")
	}
	if _, err := LoadPrivateKey(filepath.Join(dir, "e"), "pw"); err != nil {
		t.Fatal(err)
	}
}

// fakeInstall: an install dir with fake longarmd and longarm "binaries" and a signed
// release of version "2.0.0".
type fakeInstall struct {
	dir, data string
	cfg       Config
	files     []File
	content   map[string][]byte
	watchdogs []string
}

func newFakeInstall(t *testing.T) *fakeInstall {
	pub, priv, _ := GenerateKey("")
	kdir := t.TempDir()
	os.WriteFile(filepath.Join(kdir, "k"), priv, 0o600)
	sk, _ := LoadPrivateKey(filepath.Join(kdir, "k"), "")
	f := &fakeInstall{dir: t.TempDir(), data: t.TempDir(), content: map[string][]byte{}}
	for _, b := range []string{"longarmd", "longarm"} {
		os.WriteFile(filepath.Join(f.dir, exeName(b)), []byte(b+" v1"), 0o755)
		name := ReleaseName(b, runtime.GOOS, runtime.GOARCH)
		data := []byte(b + " v2")
		sum := sha256.Sum256(data)
		f.content[name] = data
		f.files = append(f.files, File{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)),
			Signature: Sign(sk, data, name, "2.0.0")})
	}
	f.cfg = Config{DataDir: f.data, Exe: filepath.Join(f.dir, exeName("longarmd")), Version: "1.0.0", PublicKey: pub,
		HealthWindow: 300 * time.Millisecond, StartGrace: 2 * time.Second, Mode: "exec", Args: []string{"run"}}
	saved := startWatchdog
	startWatchdog = func(old, dataDir string) (*exec.Cmd, error) {
		f.watchdogs = append(f.watchdogs, old)
		return exec.Command("does-not-matter"), nil
	}
	t.Cleanup(func() { startWatchdog = saved })
	return f
}

func (f *fakeInstall) fetch(_ context.Context, version, name string, w io.Writer) error {
	b, ok := f.content[name]
	if !ok {
		return errors.New("404")
	}
	_, err := io.Copy(w, bytes.NewReader(b))
	return err
}

func (f *fakeInstall) read(t *testing.T, bin string) string {
	b, err := os.ReadFile(filepath.Join(f.dir, exeName(bin)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyCheckOK(t *testing.T) {
	f := newFakeInstall(t)
	if err := Apply(context.Background(), f.cfg, "2.0.0", f.files, f.fetch); err != nil {
		t.Fatal(err)
	}
	if f.read(t, "longarmd") != "longarmd v2" || f.read(t, "longarm") != "longarm v2" {
		t.Fatal("not swapped")
	}
	if b, _ := os.ReadFile(f.cfg.Exe + ".old"); string(b) != "longarmd v1" {
		t.Fatalf("backup = %q", b)
	}
	if len(f.watchdogs) != 1 || f.watchdogs[0] != f.cfg.Exe+".old" {
		t.Fatalf("watchdog from %v", f.watchdogs)
	}
	// The new version starts: health check passes.
	cfg := f.cfg
	cfg.Version = "2.0.0"
	chk, out := Startup(cfg)
	if chk == nil || out.State != PhaseChecking {
		t.Fatalf("startup: %v %+v", chk, out)
	}
	if res := chk.Run(context.Background(), func() error { return nil }); res.State != PhaseOK {
		t.Fatalf("check: %+v", res)
	}
	// The watchdog sees ok and leaves.
	if err := Watch(context.Background(), f.data, nil); err != nil {
		t.Fatal(err)
	}
	if f.read(t, "longarmd") != "longarmd v2" {
		t.Fatal("rolled back after ok")
	}
}

func TestApplyRefusesBadFiles(t *testing.T) {
	f := newFakeInstall(t)
	files := append([]File(nil), f.files...)
	files[0].SHA256 = strings.Repeat("0", 64)
	if err := Apply(context.Background(), f.cfg, "2.0.0", files, f.fetch); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("bad hash: %v", err)
	}
	// Signed for another version (replayed signature).
	if err := Apply(context.Background(), f.cfg, "2.0.1", f.files, f.fetch); err == nil || !strings.Contains(err.Error(), "signature is for") {
		t.Fatalf("replayed signature: %v", err)
	}
	files = append([]File(nil), f.files...)
	files[1].Signature = nil
	if err := Apply(context.Background(), f.cfg, "2.0.0", files, f.fetch); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("unsigned: %v", err)
	}
	cfg := f.cfg
	cfg.PublicKey = ""
	saved := PublicKey
	PublicKey = ""
	defer func() { PublicKey = saved }()
	if err := Apply(context.Background(), cfg, "2.0.0", f.files, f.fetch); err == nil {
		t.Fatal("applied without a public key")
	}
	if f.read(t, "longarmd") != "longarmd v1" || f.read(t, "longarm") != "longarm v1" {
		t.Fatal("files changed by a refused update")
	}
	if _, err := LoadState(f.data); err != nil {
		t.Fatal(err)
	}
}

func TestWatchdogRollsBack(t *testing.T) {
	for _, tc := range []string{"unhealthy", "never-started"} {
		t.Run(tc, func(t *testing.T) {
			f := newFakeInstall(t)
			f.cfg.Exe = filepath.Join(f.dir, exeName("longarmd"))
			f.cfg.Args = nil
			if err := Apply(context.Background(), f.cfg, "2.0.0", f.files, f.fetch); err != nil {
				t.Fatal(err)
			}
			if tc == "unhealthy" {
				cfg := f.cfg
				cfg.Version = "2.0.0"
				chk, _ := Startup(cfg)
				res := chk.Run(context.Background(), func() error { return errors.New("fatal: no hub") })
				if res.State != PhaseUnhealthy {
					t.Fatalf("check: %+v", res)
				}
				// Startup recorded this test process as "the new longarmd"; the
				// watchdog would kill it.
				st, _ := LoadState(f.data)
				st.PID = 0
				st.Save(f.data)
			}
			// Exec-mode restart of the fake old binary fails (it is not an
			// executable); the files must be restored regardless.
			Watch(context.Background(), f.data, nil)
			if f.read(t, "longarmd") != "longarmd v1" || f.read(t, "longarm") != "longarm v1" {
				t.Fatalf("not rolled back: %q %q", f.read(t, "longarmd"), f.read(t, "longarm"))
			}
			if b, _ := os.ReadFile(f.cfg.Exe + ".bad"); string(b) != "longarmd v2" {
				t.Fatalf(".bad = %q", b)
			}
			st, _ := LoadState(f.data)
			if st.Phase != PhaseRolledBack {
				t.Fatalf("phase %s (%s)", st.Phase, st.Error)
			}
			if !Skipped(f.data, "2.0.0") {
				t.Fatal("version not skipped")
			}
			// The old version starts again and reports the outcome once.
			_, out := Startup(f.cfg)
			if out == nil || out.State != PhaseRolledBack || Reported(f.data) {
				t.Fatalf("outcome %+v", out)
			}
			MarkReported(f.data)
			if !Reported(f.data) {
				t.Fatal("not marked")
			}
			if err := Apply(context.Background(), f.cfg, "2.0.0", f.files, f.fetch); err == nil {
				t.Fatal("skipped version applied again")
			}
			ClearSkip(f.data, "2.0.0")
			if Skipped(f.data, "2.0.0") {
				t.Fatal("ClearSkip")
			}
		})
	}
}

// An unreadable update.json (Windows: being replaced, or scanned) does not
// end the watchdog: it retries and acts on the state once it reads.
func TestWatchdogSurvivesUnreadableState(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, StateFile), []byte("{half a fi"), 0o600)
	done := make(chan error, 1)
	go func() { done <- Watch(context.Background(), dir, nil) }()
	time.Sleep(3 * watchInterval)
	select {
	case err := <-done:
		t.Fatalf("watchdog gave up on a transiently unreadable state: %v", err)
	default:
	}
	if err := (&State{From: "1.0.0", To: "2.0.0", Phase: PhaseOK}).Save(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watchdog did not finish after the state became readable")
	}
}
