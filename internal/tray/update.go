package tray

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/update"
)

// Update banner: the tray asks the hub for its newest complete release
// (GET /releases/latest.json) and, on the user's click, downloads the MSI,
// checks it against the hub's sha256 AND the minisign signature of the
// release key built into this binary (update.PublicKey, as longarmd's
// self-update), and runs it (msiexec /i <file> /qb: the installer keeps
// the remembered MSI properties and asks for elevation itself). An MSI that
// fails a check never runs.
//
//	GET  /trayapi/update          {current, latest, available, msi, state, error}
//	POST /trayapi/update/install  {"version"}: download, verify, run

// updateCheckEvery: how often the tray asks the hub.
const updateCheckEvery = time.Hour

// maxMSISize bounds a download (the hub's record says the size).
const maxMSISize = 1 << 30

type trayUpdate struct {
	mu      sync.Mutex
	checked time.Time
	latest  string     // the hub's newest complete release ("" = none / unknown)
	msi     latestFile // its MSI for this architecture (Name "" = none)
	state   string     // "" | downloading | installing | failed
	err     string
	// hooks for tests
	client  *http.Client
	pubKey  *string
	runMSI  func(path string) error
	tempDir func() (string, error)
	goos    string
}

type latestFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type updateInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
	MSI       string `json:"msi,omitempty"`
	State     string `json:"state,omitempty"`
	Error     string `json:"error,omitempty"`
}

var releaseVersionRE = regexp.MustCompile(`^v?[0-9]+\.[0-9]+`)

func msiName() string { return "longarm-windows-" + runtime.GOARCH + ".msi" }

func (u *trayUpdate) httpClient() *http.Client {
	if u.client != nil {
		return u.client
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

func (u *trayUpdate) key() string {
	if u.pubKey != nil {
		return *u.pubKey
	}
	return update.PublicKey
}

// refreshUpdate asks the hub for its newest release (at most once per
// updateCheckEvery unless force).
func (s *Server) refreshUpdate(ctx context.Context, force bool) {
	u := &s.upd
	u.mu.Lock()
	fresh := !u.checked.IsZero() && time.Since(u.checked) < updateCheckEvery
	u.mu.Unlock()
	base := strings.TrimRight(s.hubURL(), "/")
	if (fresh && !force) || base == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/releases/latest.json", nil)
	if err != nil {
		return
	}
	resp, err := u.httpClient().Do(req)
	if err != nil {
		s.opts.Log.Debug("update check", "err", err)
		return
	}
	defer resp.Body.Close()
	var lj struct {
		Version string       `json:"version"`
		Files   []latestFile `json:"files"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&lj) != nil {
		return // an older hub: no notice
	}
	var msi latestFile
	for _, f := range lj.Files {
		if f.Name == msiName() {
			msi = f
		}
	}
	u.mu.Lock()
	u.checked, u.latest, u.msi = time.Now(), lj.Version, msi
	u.mu.Unlock()
}

func (s *Server) updateInfo() updateInfo {
	u := &s.upd
	u.mu.Lock()
	defer u.mu.Unlock()
	goos := u.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	info := updateInfo{Current: s.opts.Version, Latest: u.latest, MSI: u.msi.Name, State: u.state, Error: u.err}
	info.Available = goos == "windows" && u.msi.Name != "" && releaseVersionRE.MatchString(s.opts.Version) &&
		update.ValidVersion(u.latest) && update.Compare(u.latest, s.opts.Version) > 0
	return info
}

func (s *Server) updateState(w http.ResponseWriter, r *http.Request) {
	s.refreshUpdate(r.Context(), r.URL.Query().Get("refresh") == "1")
	writeJSON(w, s.updateInfo())
}

func (s *Server) updateInstall(w http.ResponseWriter, r *http.Request) {
	var body struct{ Version string }
	if !readBody(w, r, &body) {
		return
	}
	info := s.updateInfo()
	if !info.Available || body.Version != info.Latest {
		httpError(w, http.StatusConflict, "no such update available (refresh and try again)")
		return
	}
	u := &s.upd
	u.mu.Lock()
	if u.state == "downloading" || u.state == "installing" {
		u.mu.Unlock()
		httpError(w, http.StatusConflict, "an update is already under way")
		return
	}
	u.state, u.err = "downloading", ""
	rec, version := u.msi, u.latest
	u.mu.Unlock()
	base := strings.TrimRight(s.hubURL(), "/")
	go func() {
		err := s.installUpdate(context.Background(), base, version, rec)
		u.mu.Lock()
		defer u.mu.Unlock()
		if err != nil {
			u.state, u.err = "failed", err.Error()
			s.opts.Log.Warn("update: refused or failed", "version", version, "err", err)
			return
		}
		u.state = "installing"
		s.opts.Log.Info("update: installer started", "version", version)
	}()
	writeJSON(w, map[string]string{"state": "downloading"})
}

// installUpdate downloads version's MSI and its signature, verifies both,
// and runs it. Nothing runs unless both checks pass.
func (s *Server) installUpdate(ctx context.Context, base, version string, rec latestFile) error {
	u := &s.upd
	if rec.Size <= 0 || rec.Size > maxMSISize {
		return fmt.Errorf("the hub's record of %s has a bad size (%d)", rec.Name, rec.Size)
	}
	url := base + "/releases/" + version + "/" + rec.Name
	data, err := u.fetch(ctx, url, rec.Size)
	if err != nil {
		return fmt.Errorf("download %s: %w", rec.Name, err)
	}
	sig, err := u.fetch(ctx, url+update.SigExt, 64<<10)
	if err != nil {
		return fmt.Errorf("download the signature of %s: %w", rec.Name, err)
	}
	if err := verifyMSI(data, sig, rec.Name, version, rec.SHA256, u.key()); err != nil {
		return err
	}
	mk := u.tempDir
	if mk == nil {
		// a new directory only this user can read (0700 / the user's
		// temp directory ACL)
		mk = func() (string, error) { return os.MkdirTemp("", "longarm-update-*") }
	}
	dir, err := mk()
	if err != nil {
		return err
	}
	p := filepath.Join(dir, rec.Name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		os.RemoveAll(dir)
		return err
	}
	run := u.runMSI
	if run == nil {
		run = startMSI
	}
	if err := run(p); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("start the installer: %w", err)
	}
	return nil
}

// fetch GETs url (at most limit bytes).
func (u *trayUpdate) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("larger than the hub's release record")
	}
	return b, nil
}

// verifyMSI checks a downloaded MSI against the hub's sha256 and the
// minisign signature by the release key (pubKey) for name and version.
func verifyMSI(data, sig []byte, name, version, wantSHA256, pubKey string) error {
	if strings.TrimSpace(pubKey) == "" {
		return errors.New("this longarm-tray has no update key built in, so it can't verify the installer: download the MSI from the hub's panel (Get Longarm)")
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); wantSHA256 == "" || !strings.EqualFold(got, wantSHA256) {
		return fmt.Errorf("refusing %s: its sha256 %s doesn't match the hub's release record", name, got)
	}
	if err := update.Verify(pubKey, data, sig, name, version); err != nil {
		return fmt.Errorf("refusing %s: %w", name, err)
	}
	return nil
}

// startMSI runs the installer with a basic UI (msiexec asks for elevation
// when the install is per-machine) without waiting: it may replace this
// tray.
func startMSI(path string) error {
	if runtime.GOOS != "windows" {
		return errors.New("MSI installs are Windows only")
	}
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "msiexec.exe"), "/i", path, "/qb")
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
