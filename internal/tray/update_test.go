package tray

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/update"
)

// fakeReleaseHub serves /releases/latest.json and the MSI with its
// signature; the test can tamper with what it serves.
type fakeReleaseHub struct {
	version string
	msi     []byte // served
	record  []byte // what latest.json's sha256 and size describe
	sig     []byte
}

func (f *fakeReleaseHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := msiName()
	switch r.URL.Path {
	case "/releases/latest.json":
		sum := sha256.Sum256(f.record)
		json.NewEncoder(w).Encode(map[string]any{"version": f.version, "files": []map[string]any{
			{"name": name, "sha256": hex.EncodeToString(sum[:]), "size": len(f.record)},
			{"name": "longarmd-linux-amd64", "sha256": "00", "size": 1},
		}})
	case "/healthz":
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": "1.1.5"})
	case "/releases/" + f.version + "/" + name:
		w.Write(f.msi)
	case "/releases/" + f.version + "/" + name + update.SigExt:
		w.Write(f.sig)
	default:
		http.NotFound(w, r)
	}
}

func releaseKey(t *testing.T) (pub string, sign func(data []byte, name, version string) []byte) {
	t.Helper()
	pub, priv, err := update.GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "release.key")
	if err := os.WriteFile(p, priv, 0o600); err != nil {
		t.Fatal(err)
	}
	sk, err := update.LoadPrivateKey(p, "")
	if err != nil {
		t.Fatal(err)
	}
	return pub, func(data []byte, name, version string) []byte { return update.Sign(sk, data, name, version) }
}

// TestTrayUpdateVerify: the MSI runs only when it matches the hub's sha256
// AND carries a signature of the built-in release key for its name and
// version; a tampered file or a bad signature is refused and nothing runs.
func TestTrayUpdateVerify(t *testing.T) {
	pub, sign := releaseKey(t)
	_, signOther := releaseKey(t)
	name := msiName()
	good := []byte("the real MSI")
	cases := []struct {
		name    string
		msi     []byte
		record  []byte
		sig     []byte
		key     string
		wantErr string
	}{
		{"good", good, good, sign(good, name, "1.2.0"), pub, ""},
		{"tampered file", []byte("evil MSI!!!!"), good, sign(good, name, "1.2.0"), pub, "sha256"},
		{"tampered file and record", []byte("evil MSI"), []byte("evil MSI"), sign(good, name, "1.2.0"), pub, "invalid signature"},
		{"other key", good, good, signOther(good, name, "1.2.0"), pub, "invalid signature"},
		{"replayed old version", good, good, sign(good, name, "1.1.0"), pub, "refusing"},
		{"another file's signature", good, good, sign(good, "longarm-linux-amd64.deb", "1.2.0"), pub, "refusing"},
		{"garbage signature", good, good, []byte("not a signature"), pub, "refusing"},
		{"no key in this build", good, good, sign(good, name, "1.2.0"), "", "no update key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := httptest.NewServer(&fakeReleaseHub{version: "1.2.0", msi: c.msi, record: c.record, sig: c.sig})
			defer hub.Close()
			key := c.key
			tmp := t.TempDir()
			var ran []string
			s := &Server{opts: Options{Version: "1.1.0", HubURL: hub.URL + "/", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
			s.upd.pubKey, s.upd.goos = &key, "windows"
			s.upd.runMSI = func(p string) error { ran = append(ran, p); return nil }
			s.upd.tempDir = func() (string, error) { return os.MkdirTemp(tmp, "u-*") }
			s.refreshUpdate(context.Background(), true)
			info := s.updateInfo()
			if !info.Available || info.Latest != "1.2.0" || info.MSI != name {
				t.Fatalf("update info: %+v", info)
			}
			s.upd.mu.Lock()
			rec := s.upd.msi
			s.upd.mu.Unlock()
			err := s.installUpdate(context.Background(), hub.URL, "1.2.0", rec)
			if c.wantErr == "" {
				if err != nil || len(ran) != 1 {
					t.Fatalf("good MSI: %v, ran %v", err, ran)
				}
				if b, _ := os.ReadFile(ran[0]); string(b) != string(good) || filepath.Base(ran[0]) != name {
					t.Fatalf("ran %s with %q", ran[0], b)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err %v, want %q", err, c.wantErr)
			}
			if len(ran) != 0 {
				t.Fatalf("an unverified MSI ran: %v", ran)
			}
			if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
				t.Fatalf("the refused download was written: %v", ents)
			}
		})
	}
}

// TestTrayUpdateInfo: the banner shows only for a newer release with an MSI
// for this machine, on Windows, for a released build.
func TestTrayUpdateInfo(t *testing.T) {
	hub := httptest.NewServer(&fakeReleaseHub{version: "1.2.0", record: []byte("x")})
	defer hub.Close()
	for _, c := range []struct {
		current, goos string
		want          bool
	}{
		{"1.1.0", "windows", true}, {"1.2.0", "windows", false}, {"1.3.0", "windows", false}, {"dev", "windows", false}, {"1.1.0", "linux", false},
	} {
		s := &Server{opts: Options{Version: c.current, HubURL: hub.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
		s.upd.goos = c.goos
		s.refreshUpdate(context.Background(), false)
		if got := s.updateInfo(); got.Available != c.want || got.Hub != "1.1.5" || got.Latest != "1.2.0" {
			t.Errorf("%s on %s: %+v", c.current, c.goos, got)
		}
	}
	// no hub configured: no request, no banner
	s := &Server{opts: Options{Version: "1.0.0", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	s.refreshUpdate(context.Background(), true)
	if s.updateInfo().Available {
		t.Fatal("banner without a hub")
	}
}

// TestTrayUpdateChannel: the banner asks for the newest release of the
// machine's channel and asks again as soon as the channel changes.
func TestTrayUpdateChannel(t *testing.T) {
	var asked []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/latest.json" {
			http.NotFound(w, r) // /healthz: the hub's version, not this test's business
			return
		}
		ch := r.URL.Query().Get("channel")
		asked = append(asked, ch)
		v := map[string]string{"": "0.1.20", "preview": "0.1.21-rc.1", "dev": "0.1.22-dev.7"}[ch]
		json.NewEncoder(w).Encode(map[string]any{"version": v, "files": []map[string]any{{"name": msiName(), "sha256": "00", "size": 1}}})
	}))
	defer hub.Close()
	channel := "dev"
	s := &Server{opts: Options{Version: "0.1.20", HubURL: hub.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	s.upd.goos = "windows"
	s.upd.chanFn = func() string { return channel }
	s.refreshUpdate(context.Background(), false)
	if got := s.updateInfo(); !got.Available || got.Latest != "0.1.22-dev.7" || got.Channel != "dev" {
		t.Fatalf("dev: %+v", got)
	}
	s.refreshUpdate(context.Background(), false) // fresh: no request
	channel = "beta"
	s.refreshUpdate(context.Background(), false) // the channel changed: asks again
	if got := s.updateInfo(); got.Latest != "0.1.21-rc.1" || got.Channel != "preview" {
		t.Fatalf("preview: %+v", got)
	}
	channel = ""
	s.refreshUpdate(context.Background(), false)
	if got := s.updateInfo(); got.Available || got.Channel != "stable" {
		t.Fatalf("stable, running 0.1.20: %+v", got)
	}
	if strings.Join(asked, ",") != "dev,preview," {
		t.Fatalf("asked %q", asked)
	}
}
