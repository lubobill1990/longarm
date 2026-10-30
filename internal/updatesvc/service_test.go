package updatesvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/update"
)

// ---------------------------------------------------------------- fakes

type fakeSource struct {
	mu       sync.Mutex
	releases map[string]*Release
	latest   string
	latestBy map[string]string // channel -> version (else latest)
	channels []string          // Latest was asked for
	files    map[string][]byte // version/name
	gets     int
	hold     chan struct{} // Download waits for it (nil = no wait)
}

func (f *fakeSource) Latest(ctx context.Context, channel string) (*Release, error) {
	f.mu.Lock()
	f.channels = append(f.channels, channel)
	v := f.latest
	if l := f.latestBy[channel]; l != "" {
		v = l
	}
	f.mu.Unlock()
	return f.Get(ctx, v)
}

func (f *fakeSource) Get(_ context.Context, v string) (*Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	r := f.releases[v]
	if r == nil {
		return nil, ErrNoRelease
	}
	c := *r
	return &c, nil
}

func (f *fakeSource) Download(ctx context.Context, v, name string, w io.Writer, limit int64) error {
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	b, ok := f.files[v+"/"+name]
	f.mu.Unlock()
	if !ok {
		return ErrNoRelease
	}
	if int64(len(b)) > limit {
		return errors.New("too large")
	}
	_, err := w.Write(b)
	return err
}

type fakeWorkspace struct {
	root    string
	removed []string
	locked  []string
	failErr error
}

func (w *fakeWorkspace) Prepare(v string) (string, string, error) {
	if w.failErr != nil {
		return "", "", w.failErr
	}
	d := filepath.Join(w.root, v)
	os.RemoveAll(d)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", "", err
	}
	return d, filepath.Join(w.root, "install-"+v+".log"), nil
}

func (w *fakeWorkspace) Lock(p string) (io.ReadCloser, error) {
	w.locked = append(w.locked, p)
	return os.Open(p)
}

func (w *fakeWorkspace) Remove(d string) {
	w.removed = append(w.removed, d)
	os.RemoveAll(d)
}

type fakeLauncher struct {
	mu       sync.Mutex
	launched [][2]string
	outcomes map[string]int // log -> exit code (absent: still running)
	err      error
	forgot   int
}

func (l *fakeLauncher) Launch(msi, log string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.launched = append(l.launched, [2]string{msi, log})
	return nil
}

func (l *fakeLauncher) Outcome(log string) (int, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.outcomes[log]
	return c, ok, nil
}

func (l *fakeLauncher) Forget() { l.mu.Lock(); l.forgot++; l.mu.Unlock() }

func (l *fakeLauncher) count() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.launched) }

type fakeSettings struct {
	mu      sync.Mutex
	hub     string
	owner   string
	auto    bool
	hours   int
	channel string
}

func (s *fakeSettings) Hub() (string, error) {
	if s.hub == "" {
		return "", errors.New("none")
	}
	return s.hub, nil
}
func (s *fakeSettings) Owner() string { return s.owner }
func (s *fakeSettings) Auto() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auto, s.hours
}
func (s *fakeSettings) Channel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channel
}
func (s *fakeSettings) SetChannel(c string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channel = c
	return nil
}
func (s *fakeSettings) SetAuto(on bool, h int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auto, s.hours = on, h
	return nil
}

type memStore struct {
	mu sync.Mutex
	st *State
}

func (m *memStore) Load() (*State, error) { m.mu.Lock(); defer m.mu.Unlock(); return m.st, nil }
func (m *memStore) Save(s *State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *s
	m.st = &c
	return nil
}

// ---------------------------------------------------------------- setup

const ownerSID = "S-1-5-21-1-2-3-1001"

type rig struct {
	t        *testing.T
	pub      string
	sign     func(data []byte, name, version string) []byte
	src      *fakeSource
	ws       *fakeWorkspace
	launcher *fakeLauncher
	settings *fakeSettings
	store    *memStore
	hubs     []string // NewSource was called with
	svc      *Service
	cfg      Config
	ctx      context.Context
}

func newRig(t *testing.T, installed string) *rig {
	t.Helper()
	pub, privFile, err := update.GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	kf := filepath.Join(t.TempDir(), "k")
	os.WriteFile(kf, privFile, 0o600)
	sk, err := update.LoadPrivateKey(kf, "")
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, pub: pub,
		sign:     func(d []byte, n, v string) []byte { return update.Sign(sk, d, n, v) },
		src:      &fakeSource{releases: map[string]*Release{}, files: map[string][]byte{}},
		ws:       &fakeWorkspace{root: t.TempDir()},
		launcher: &fakeLauncher{outcomes: map[string]int{}},
		settings: &fakeSettings{hub: "hub.example.com", owner: ownerSID},
		store:    &memStore{},
	}
	r.cfg = Config{Version: installed, Arch: "amd64", PublicKey: pub, Settings: r.settings,
		NewSource: func(hub string) (Source, error) { r.hubs = append(r.hubs, hub); return r.src, nil },
		Workspace: r.ws, Launcher: r.launcher, Store: r.store, WatchEvery: 10 * time.Millisecond,
		InstallEvery: time.Millisecond, InstallBurst: 100}
	ctx, cancel := context.WithCancel(context.Background())
	r.ctx = ctx
	r.renew()
	t.Cleanup(func() {
		cancel()
		r.svc.Wait()
	})
	return r
}

// renew starts a new Service with r.cfg (the old one is left alone: tests
// use it no further).
func (r *rig) renew() {
	r.svc = New(r.cfg)
	r.svc.Start(r.ctx)
}

// publish adds a release with an MSI for arch, signed (sig != nil) as
// given; data is the package.
func (r *rig) publish(version, arch string, data, sig []byte, latest bool) {
	name := update.MSIName(arch)
	sum := sha256.Sum256(data)
	r.src.releases[version] = &Release{Version: version, Channel: "stable",
		Files: []ReleaseFile{{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)), Signed: sig != nil}}}
	r.src.files[version+"/"+name] = data
	if sig != nil {
		r.src.files[version+"/"+name+update.SigExt] = sig
	}
	if latest {
		r.src.latest = version
	}
}

func (r *rig) good(version string) []byte {
	data := []byte("MSI package " + version + strings.Repeat("x", 1000))
	r.publish(version, "amd64", data, r.sign(data, update.MSIName("amd64"), version), true)
	return data
}

var user = Caller{SID: "S-1-5-21-1-2-3-2002"}

func (r *rig) install(c Caller, req Request) *Response {
	req.Op = OpInstall
	return r.svc.Handle(context.Background(), c, &req)
}

func expectRefused(t *testing.T, resp *Response, code string, launcher *fakeLauncher) {
	t.Helper()
	if resp.OK || resp.Code != code {
		t.Fatalf("want refused %s, got %+v", code, resp)
	}
	if launcher.count() != 0 {
		t.Fatalf("the installer was launched for a refused package")
	}
}

// ---------------------------------------------------------------- tests

func TestInstallVerifiedPackage(t *testing.T) {
	r := newRig(t, "0.1.8")
	r.good("0.1.9")
	resp := r.install(user, Request{Latest: true, Requester: "longarm worker update by owner@example.com"})
	if !resp.OK || resp.Phase != PhaseInstalling || resp.From != "0.1.8" || resp.To != "0.1.9" {
		t.Fatalf("install: %+v", resp)
	}
	if r.launcher.count() != 1 {
		t.Fatalf("launched %d times", r.launcher.count())
	}
	l := r.launcher.launched[0]
	if filepath.Base(l[0]) != "longarm-windows-amd64.msi" || !strings.HasSuffix(l[1], "install-0.1.9.log") {
		t.Fatalf("launch %v", l)
	}
	if len(r.ws.locked) != 1 || r.ws.locked[0] != l[0] {
		t.Fatalf("the package was not verified through a locked handle: %v", r.ws.locked)
	}
	if r.store.st == nil || r.store.st.Pending == nil || r.store.st.Pending.To != "0.1.9" {
		t.Fatalf("pending not saved before the launch: %+v", r.store.st)
	}
	if st := r.svc.Status(); st.State != PhaseInstalling || st.Target != "0.1.9" {
		t.Fatalf("status %+v", st)
	}
	// a second request while it runs: serialized
	if resp := r.install(user, Request{Version: "0.1.9"}); resp.OK || resp.Code != CodeBusy {
		t.Fatalf("second install: %+v", resp)
	}
	// msiexec ends (the old service saw it: no restart happened)
	r.launcher.mu.Lock()
	r.launcher.outcomes[l[1]] = 0
	r.launcher.mu.Unlock()
	waitFor(t, func() bool { st := r.svc.Status(); return st.State == PhaseIdle && st.Last != nil })
	if st := r.svc.Status(); !st.Last.OK || st.Last.To != "0.1.9" || !strings.Contains(st.Last.By, "owner@example.com") {
		t.Fatalf("result %+v", st.Last)
	}
	if len(r.ws.removed) == 0 {
		t.Fatal("the download dir was not removed")
	}
	if r.hubs[0] != "hub.example.com" {
		t.Fatalf("source for %v", r.hubs)
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestUnsignedPackageRefused(t *testing.T) {
	r := newRig(t, "0.1.8")
	data := []byte("unsigned")
	r.publish("0.1.9", "amd64", data, nil, true)
	expectRefused(t, r.install(user, Request{Latest: true}), CodeInvalid, r.launcher)
}

func TestTamperedPackageRefused(t *testing.T) {
	r := newRig(t, "0.1.8")
	data := []byte("the real package")
	sig := r.sign(data, update.MSIName("amd64"), "0.1.9")
	// the hub serves other bytes, its record matching them: the signature fails
	evil := []byte("a tampered package")
	r.publish("0.1.9", "amd64", evil, sig, true)
	expectRefused(t, r.install(user, Request{Latest: true}), CodeInvalid, r.launcher)

	// the record's sha256 differs from the bytes served
	r2 := newRig(t, "0.1.8")
	r2.good("0.1.9")
	r2.src.releases["0.1.9"].Files[0].SHA256 = strings.Repeat("0", 64)
	expectRefused(t, r2.install(user, Request{Latest: true}), CodeInvalid, r2.launcher)

	// signed with another key
	r3 := newRig(t, "0.1.8")
	other := newRig(t, "0.1.8")
	data3 := []byte("package")
	r3.publish("0.1.9", "amd64", data3, other.sign(data3, update.MSIName("amd64"), "0.1.9"), true)
	expectRefused(t, r3.install(user, Request{Latest: true}), CodeInvalid, r3.launcher)
}

func TestTrustedCommentMismatchRefused(t *testing.T) {
	for _, tc := range []struct{ name, version string }{
		{"longarm-windows-arm64.msi", "0.1.9"}, // another file's signature
		{"longarm-windows-amd64.msi", "0.1.7"}, // an older version's signature (replay)
		{"longarm-windows-amd64.zip", "0.1.9"},
	} {
		r := newRig(t, "0.1.8")
		data := []byte("package")
		r.publish("0.1.9", "amd64", data, r.sign(data, tc.name, tc.version), true)
		expectRefused(t, r.install(user, Request{Latest: true}), CodeInvalid, r.launcher)
	}
}

func TestDowngradeAndSameVersion(t *testing.T) {
	r := newRig(t, "0.1.9")
	r.good("0.1.8")
	expectRefused(t, r.install(user, Request{Version: "0.1.8"}), CodeRefused, r.launcher)
	expectRefused(t, r.install(Caller{SID: "S-1-5-32-544", Admin: true}, Request{Version: "0.1.8"}), CodeRefused, r.launcher)

	r.good("0.1.9")
	if resp := r.install(user, Request{Latest: true}); !resp.OK || resp.Phase != PhaseUpToDate || r.launcher.count() != 0 {
		t.Fatalf("same version: %+v", resp)
	}
	// a reinstall: a local administrator only
	expectRefused(t, r.install(user, Request{Version: "0.1.9", Reinstall: true}), CodeDenied, r.launcher)
	expectRefused(t, r.install(Caller{SID: systemSIDForTest, System: true}, Request{Version: "0.1.9", Reinstall: true}), CodeDenied, r.launcher)
	if resp := r.install(Caller{SID: "S-1-5-21-9", Admin: true}, Request{Version: "0.1.9", Reinstall: true}); !resp.OK || resp.Phase != PhaseInstalling {
		t.Fatalf("admin reinstall: %+v", resp)
	}
	// a dev build installs nothing
	d := newRig(t, "dev")
	d.good("0.1.9")
	expectRefused(t, d.install(user, Request{Latest: true}), CodeRefused, d.launcher)
}

const systemSIDForTest = "S-1-5-18"

func TestOtherArchitectureRefused(t *testing.T) {
	r := newRig(t, "0.1.8")
	data := []byte("arm package")
	r.publish("0.1.9", "arm64", data, r.sign(data, update.MSIName("arm64"), "0.1.9"), true)
	expectRefused(t, r.install(user, Request{Latest: true}), CodeNotFound, r.launcher)
	// the arm64 MSI renamed to the amd64 name: its signature names arm64
	r.publish("0.1.9", "amd64", data, r.sign(data, update.MSIName("arm64"), "0.1.9"), true)
	expectRefused(t, r.install(user, Request{Latest: true}), CodeInvalid, r.launcher)
}

func TestNoKeyNoHub(t *testing.T) {
	r := newRig(t, "0.1.8")
	r.good("0.1.9")
	r.cfg.PublicKey = ""
	nk := New(r.cfg)
	if resp := nk.Handle(context.Background(), user, &Request{Op: OpInstall, Latest: true}); resp.Code != CodeUnavailable {
		t.Fatalf("no key: %+v", resp)
	}
	r2 := newRig(t, "0.1.8")
	r2.settings.hub = ""
	expectRefused(t, r2.install(user, Request{Latest: true}), CodeUnavailable, r2.launcher)
}

// TestHubFromRequestIgnored: a request can't name a hub - the pipe refuses
// unknown fields, and the source is always the recorded hub's.
func TestHubFromRequestIgnored(t *testing.T) {
	r := newRig(t, "0.1.8")
	r.good("0.1.9")
	for _, line := range []string{
		`{"op":"install","latest":true,"hub":"evil.example.com"}`,
		`{"op":"install","latest":true,"url":"https://evil.example.com/x.msi"}`,
		`{"op":"install","latest":true,"public_key":"RWQ..."}`,
	} {
		resp := pipeRoundTrip(t, r.svc, user, line)
		if resp.OK || resp.Code != CodeInvalid {
			t.Fatalf("%s: %+v", line, resp)
		}
	}
	if len(r.hubs) != 0 || r.launcher.count() != 0 {
		t.Fatalf("a refused request reached the hub %v / the installer", r.hubs)
	}
	resp := pipeRoundTrip(t, r.svc, user, `{"op":"install","latest":true,"requester":"x"}`)
	if !resp.OK {
		t.Fatalf("install: %+v", resp)
	}
	if len(r.hubs) != 1 || r.hubs[0] != "hub.example.com" {
		t.Fatalf("sources: %v", r.hubs)
	}
}

// pipeRoundTrip serves one raw request line over an in-memory connection.
func pipeRoundTrip(t *testing.T, s *Service, c Caller, line string) *Response {
	t.Helper()
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		serveConn(context.Background(), a, s, func(net.Conn) (Caller, error) { return c, nil })
		close(done)
	}()
	b.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := b.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := readMessage(b, &resp, false); err != nil {
		t.Fatal(err)
	}
	b.Close()
	<-done
	return &resp
}

func TestRateLimits(t *testing.T) {
	r := newRig(t, "0.1.8")
	r.cfg.InstallEvery, r.cfg.InstallBurst = time.Hour, 2
	r.cfg.RequestRate, r.cfg.RequestBurst = 0.001, 5
	r.renew()
	s := r.svc
	// installs: everybody together
	for i := 0; i < 2; i++ {
		if resp := r.install(Caller{SID: fmt.Sprintf("S-1-5-21-%d", i)}, Request{Latest: true}); resp.Code == CodeRateLimited {
			t.Fatalf("install %d limited", i)
		}
	}
	if resp := r.install(Caller{SID: "S-1-5-21-77"}, Request{Latest: true}); resp.Code != CodeRateLimited {
		t.Fatalf("third install: %+v", resp)
	}
	// requests: per caller
	limited := 0
	for i := 0; i < 10; i++ {
		if resp := s.Handle(context.Background(), user, &Request{Op: OpStatus}); resp.Code == CodeRateLimited {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("no request was rate limited")
	}
	if resp := s.Handle(context.Background(), Caller{SID: "S-1-5-21-99"}, &Request{Op: OpStatus}); !resp.OK {
		t.Fatalf("another caller is limited too: %+v", resp)
	}
}

func TestBusySerializesInstalls(t *testing.T) {
	r := newRig(t, "0.1.8")
	r.good("0.1.9")
	r.src.hold = make(chan struct{})
	first := make(chan *Response, 1)
	go func() { first <- r.install(user, Request{Latest: true}) }()
	waitFor(t, func() bool { return r.svc.Status().State == PhaseDownloading })
	if resp := r.install(user, Request{Version: "0.1.9"}); resp.Code != CodeBusy {
		t.Fatalf("concurrent install: %+v", resp)
	}
	close(r.src.hold)
	if resp := <-first; !resp.OK || resp.Phase != PhaseInstalling {
		t.Fatalf("first: %+v", resp)
	}
	if r.launcher.count() != 1 {
		t.Fatalf("launched %d", r.launcher.count())
	}
}

func TestSetAutoOwnerOrAdminOnly(t *testing.T) {
	r := newRig(t, "0.1.8")
	set := func(c Caller, on bool) *Response {
		return r.svc.Handle(context.Background(), c, &Request{Op: OpSetAuto, Auto: on, IntervalHours: 3})
	}
	if resp := set(user, true); resp.Code != CodeDenied {
		t.Fatalf("another user: %+v", resp)
	}
	if on, _ := r.settings.Auto(); on {
		t.Fatal("a non-owner turned auto-update on")
	}
	if resp := set(Caller{SID: ownerSID}, true); !resp.OK || !resp.Status.Auto || resp.Status.AutoIntervalHours != 3 {
		t.Fatalf("owner: %+v", resp)
	}
	if resp := set(Caller{SID: "S-1-5-21-5", Admin: true}, false); !resp.OK {
		t.Fatalf("admin: %+v", resp)
	}
	if resp := set(Caller{SID: systemSIDForTest, System: true}, true); !resp.OK {
		t.Fatalf("SYSTEM (the admin worker, for its owner): %+v", resp)
	}
	// without a recorded owner: administrators only
	r.settings.owner = ""
	if resp := set(Caller{SID: ownerSID}, false); resp.Code != CodeDenied {
		t.Fatalf("no owner recorded: %+v", resp)
	}
}

// TestSetChannel: the owner (or an administrator) picks the release
// channel; "install latest" then takes that channel's newest release.
func TestSetChannel(t *testing.T) {
	r := newRig(t, "0.1.20")
	set := func(c Caller, ch string) *Response {
		return r.svc.Handle(context.Background(), c, &Request{Op: OpSetChannel, Channel: ch})
	}
	if st := r.svc.Status(); st.Channel != "stable" {
		t.Fatalf("default channel: %q", st.Channel)
	}
	if resp := set(user, "dev"); resp.Code != CodeDenied || r.settings.Channel() != "" {
		t.Fatalf("another user: %+v", resp)
	}
	if resp := set(Caller{SID: ownerSID}, "canary"); resp.Code != CodeInvalid {
		t.Fatalf("unknown channel: %+v", resp)
	}
	if resp := set(Caller{SID: "S-1-5-21-5", Admin: true}, "beta"); !resp.OK || resp.Status.Channel != "preview" {
		t.Fatalf("admin, beta: %+v", resp)
	}
	if resp := set(Caller{SID: ownerSID}, "dev"); !resp.OK || resp.Status.Channel != "dev" || r.settings.Channel() != "dev" {
		t.Fatalf("owner: %+v", resp)
	}
	r.good("0.1.21")                                              // the latest stable
	data := []byte("MSI package dev" + strings.Repeat("x", 1000)) // and a newer dev build
	r.publish("0.1.22-dev.7", "amd64", data, r.sign(data, update.MSIName("amd64"), "0.1.22-dev.7"), false)
	r.src.mu.Lock()
	r.src.latestBy = map[string]string{"dev": "0.1.22-dev.7"}
	r.src.mu.Unlock()
	resp := r.install(user, Request{Latest: true})
	if !resp.OK || resp.To != "0.1.22-dev.7" {
		t.Fatalf("install latest on dev: %+v", resp)
	}
	r.src.mu.Lock()
	asked := slices.Clone(r.src.channels)
	r.src.mu.Unlock()
	if len(asked) == 0 || asked[len(asked)-1] != "dev" {
		t.Fatalf("Latest asked for %v", asked)
	}
}

func TestClientDetectsImpostor(t *testing.T) {
	r := newRig(t, "0.1.8")
	served := 0
	c := &Client{
		Dial: func(context.Context) (net.Conn, error) {
			a, b := net.Pipe()
			go func() {
				served++
				serveConn(context.Background(), a, r.svc, func(net.Conn) (Caller, error) { return user, nil })
			}()
			return b, nil
		},
		VerifyServer: func(net.Conn) error { return fmt.Errorf("%w: pid 4242 is C:\\Users\\x\\fake.exe", ErrImpostor) },
	}
	_, err := c.Do(context.Background(), &Request{Op: OpInstall, Latest: true})
	if !errors.Is(err, ErrImpostor) {
		t.Fatalf("err %v", err)
	}
	if len(r.hubs) != 0 {
		t.Fatal("the request reached the (fake) server")
	}
	c.VerifyServer = func(net.Conn) error { return nil }
	st, err := c.Status(context.Background())
	if err != nil || st.Installed != "0.1.8" {
		t.Fatalf("status %+v %v", st, err)
	}
	// no check at all is refused too
	c.VerifyServer = nil
	if _, err := c.Do(context.Background(), &Request{Op: OpStatus}); err == nil {
		t.Fatal("a client without a server check went on")
	}
}

func TestLaunchFailureAndReconcile(t *testing.T) {
	r := newRig(t, "0.1.8")
	r.good("0.1.9")
	r.launcher.err = errors.New("schtasks: access denied")
	resp := r.install(user, Request{Latest: true})
	if resp.OK || resp.Code != CodeFailed {
		t.Fatalf("%+v", resp)
	}
	if st := r.svc.Status(); st.Last == nil || st.Last.OK || st.State != PhaseIdle {
		t.Fatalf("status %+v", st)
	}

	// the new service after a successful upgrade
	n := newRig(t, "0.1.9")
	n.store.st = &State{Pending: &Pending{From: "0.1.8", To: "0.1.9", Dir: filepath.Join(n.ws.root, "0.1.9"), Log: "l", Started: time.Now()}}
	n.renew()
	if st := n.svc.Status(); st.Last == nil || !st.Last.OK || st.Last.To != "0.1.9" || n.store.st.Pending != nil {
		t.Fatalf("reconciled %+v", st.Last)
	}
	// the old service again after a failed install (rolled back)
	o := newRig(t, "0.1.8")
	o.launcher.outcomes["l"] = 1603
	o.store.st = &State{Pending: &Pending{From: "0.1.8", To: "0.1.9", Log: "l", Started: time.Now()}}
	o.renew()
	if st := o.svc.Status(); st.Last == nil || st.Last.OK || st.Last.Code != 1603 {
		t.Fatalf("failed install %+v", st.Last)
	}
	// no outcome long after: failed
	x := newRig(t, "0.1.8")
	x.store.st = &State{Pending: &Pending{From: "0.1.8", To: "0.1.9", Log: "l", Started: time.Now().Add(-3 * time.Hour)}}
	x.renew()
	if st := x.svc.Status(); st.Last == nil || st.Last.OK {
		t.Fatalf("timed out install %+v", st.Last)
	}
}

func TestAutoUpdateWaitsForQuiet(t *testing.T) {
	r := newRig(t, "0.1.8")
	r.good("0.1.9")
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	r.cfg.Now = func() time.Time { return now }
	r.renew()
	r.svc.Handle(context.Background(), user, &Request{Op: OpActivity}) // a job just started
	if retry := r.svc.autoStep(context.Background(), now); !retry || r.launcher.count() != 0 {
		t.Fatal("installed right after a job started")
	}
	now = now.Add(6 * time.Minute)
	if retry := r.svc.autoStep(context.Background(), now.Add(-time.Minute)); retry || r.launcher.count() != 1 {
		t.Fatalf("not installed when quiet (launched %d)", r.launcher.count())
	}
	// busy machine: at most MaxDefer later
	b := newRig(t, "0.1.8")
	b.good("0.1.9")
	b.cfg.Now = func() time.Time { return now }
	b.renew()
	b.svc.Handle(context.Background(), user, &Request{Op: OpActivity})
	if retry := b.svc.autoStep(context.Background(), now.Add(-25*time.Hour)); retry || b.launcher.count() != 1 {
		t.Fatal("deferred past MaxDefer")
	}
}

// ---------------------------------------------------------------- workspace

type fakeOps struct {
	entries   map[string]dirInfo
	untrusted map[string]bool
	removed   []string
	created   []string
}

func (f *fakeOps) Inspect(p string) (dirInfo, error) { return f.entries[p], nil }
func (f *fakeOps) Create(p string) error {
	f.created = append(f.created, p)
	f.entries[p] = dirInfo{Exists: true, Dir: true, Secured: true}
	return nil
}
func (f *fakeOps) Remove(p string) error {
	f.removed = append(f.removed, p)
	delete(f.entries, p)
	return nil
}
func (f *fakeOps) Trusted(p string) error {
	if f.untrusted[p] {
		return errors.New("writable by users")
	}
	return nil
}
func (f *fakeOps) Lock(string) (io.ReadCloser, error) { return io.NopCloser(&bytes.Buffer{}), nil }

func TestWorkspaceChecks(t *testing.T) {
	root := filepath.FromSlash("/pd/Longarm")
	up := filepath.Join(root, UpdatesDirName)
	for name, before := range map[string]dirInfo{
		"reparse point (junction to elsewhere)": {Exists: true, Dir: true, Reparse: true, Secured: true},
		"a file":                                {Exists: true},
		"other ACEs / owner":                    {Exists: true, Dir: true},
	} {
		ops := &fakeOps{entries: map[string]dirInfo{up: before}}
		w := &FSWorkspace{Root: root, ops: ops}
		dir, log, err := w.Prepare("0.1.9")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(ops.removed) == 0 || ops.removed[0] != up || ops.created[0] != up {
			t.Fatalf("%s: not removed and made anew: removed %v created %v", name, ops.removed, ops.created)
		}
		if dir != filepath.Join(up, "0.1.9") || log != filepath.Join(up, "install-0.1.9.log") {
			t.Fatalf("%s: %s %s", name, dir, log)
		}
	}
	// a secured dir is kept; a stale per-version dir and an old log go
	ops := &fakeOps{entries: map[string]dirInfo{
		up:                                     {Exists: true, Dir: true, Secured: true},
		filepath.Join(up, "0.1.9"):             {Exists: true, Dir: true, Reparse: true},
		filepath.Join(up, "install-0.1.9.log"): {Exists: true},
	}}
	w := &FSWorkspace{Root: root, ops: ops}
	if _, _, err := w.Prepare("0.1.9"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ops.removed, ",") != filepath.Join(up, "0.1.9")+","+filepath.Join(up, "install-0.1.9.log") {
		t.Fatalf("removed %v", ops.removed)
	}
	// an untrusted parent: nothing is made
	bad := &fakeOps{entries: map[string]dirInfo{}, untrusted: map[string]bool{root: true}}
	if _, _, err := (&FSWorkspace{Root: root, ops: bad}).Prepare("0.1.9"); err == nil || len(bad.created) != 0 {
		t.Fatalf("untrusted parent: %v %v", err, bad.created)
	}
	// a version is never a path
	if _, _, err := w.Prepare(`..\..\Windows`); err == nil {
		t.Fatal("a path as version")
	}
	// Remove: only below updates
	ops.removed = nil
	w.Remove(filepath.Join(root, "longarmd-admin"))
	w.Remove(up)
	w.Remove(filepath.Join(up, "0.1.9", "x"))
	if len(ops.removed) != 0 {
		t.Fatalf("removed %v", ops.removed)
	}
	w.Remove(filepath.Join(up, "0.1.9"))
	if len(ops.removed) != 1 {
		t.Fatalf("removed %v", ops.removed)
	}
}

func TestInstallTaskXML(t *testing.T) {
	x, err := installTaskXML(`C:\Windows\System32\msiexec.exe`, `C:\Windows\System32\sc.exe`,
		`C:\ProgramData\Longarm\updates\0.1.9\longarm-windows-amd64.msi`, `C:\ProgramData\Longarm\updates\install-0.1.9.log`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<UserId>S-1-5-18</UserId>", "<RunLevel>HighestAvailable</RunLevel>",
		`<Command>C:\Windows\System32\msiexec.exe</Command>`,
		`<Arguments>/i &#34;C:\ProgramData\Longarm\updates\0.1.9\longarm-windows-amd64.msi&#34; /qn /norestart /l*v &#34;C:\ProgramData\Longarm\updates\install-0.1.9.log&#34;</Arguments>`,
		`<Command>C:\Windows\System32\sc.exe</Command>`, "<Arguments>start LongarmUpdate</Arguments>",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("task XML lacks %s", want)
		}
	}
	if strings.Contains(x, "<Triggers>") {
		t.Error("the task has a trigger: it must run on demand only")
	}
	if _, err := installTaskXML(`C:\m.exe`, `C:\sc.exe`, `C:\x" /i "\\evil\share\a.msi`, `C:\l.log`); err == nil {
		t.Error("a quote in a path")
	}
}

func TestMSILogOutcome(t *testing.T) {
	ansi := []byte("MSI (s) (A0:B4) [12:00:00:000]: Product: Longarm -- Installation completed successfully.\r\n" +
		"MSI (s) (A0:B4) [12:00:00:001]: MainEngineThread is returning 0\r\n")
	if c, done := msiLogOutcome(ansi); !done || c != 0 {
		t.Fatalf("ansi: %d %v", c, done)
	}
	if _, done := msiLogOutcome([]byte("=== Verbose logging started ===\r\nAction start InstallFiles\r\n")); done {
		t.Fatal("running install reported done")
	}
	s := "MSI (s): MainEngineThread is returning 1603\r\n"
	u := []byte{0xff, 0xfe}
	for _, r := range s {
		u = append(u, byte(r), 0)
	}
	if c, done := msiLogOutcome(u); !done || c != 1603 {
		t.Fatalf("utf-16: %d %v", c, done)
	}
}

func TestHubBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"longarm.example.com":               "https://longarm.example.com",
		"longarm.example.com:7443":          "https://longarm.example.com",
		"https://longarm.example.com":       "https://longarm.example.com",
		"https://longarm.example.com:8443/": "https://longarm.example.com:8443",
		"grpcs://hub.example.com:7443":      "https://hub.example.com",
		"[::1]:7443":                        "https://[::1]",
	} {
		if got, err := HubBaseURL(in); err != nil || got != want {
			t.Errorf("%s: %s %v, want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "http://hub.example.com", "https://hub.example.com/x", "a b", "hub/x", "user@hub"} {
		if _, err := HubBaseURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
