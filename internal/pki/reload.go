package pki

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Reloader serves longarmd's TLS configuration from a bundle directory and picks
// up a re-issued certificate (or CA) without a restart: Watch polls the
// files and swaps the configuration atomically; handshakes already in
// progress or established connections are unaffected. A bundle that fails to
// load or is not a worker certificate is ignored (the previous one stays).
type Reloader struct {
	dir string
	log *slog.Logger

	mu     sync.RWMutex
	cfg    *tls.Config
	bundle *Bundle
	stamp  string
	live   *Bundle // see LiveBundle
	trust  *TrustSource
}

// SetTrust makes the server demand end-to-end trust of clients whenever
// t's policy is active (call before LiveBundle and serving).
func (r *Reloader) SetTrust(t *TrustSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trust = t
	r.bundle.Trust = t
	if cfg, err := r.bundle.ServerConfig(); err == nil {
		r.cfg = cfg
	}
}

// NewReloader loads the bundle in dir; it must hold a worker certificate.
func NewReloader(dir string, log *slog.Logger) (*Reloader, error) {
	if log == nil {
		log = slog.Default()
	}
	r := &Reloader{dir: dir, log: log}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reloader) fileStamp() string {
	s := ""
	for _, f := range []string{CACertFile, NodeCertFile, NodeKeyFile} {
		fi, err := os.Stat(filepath.Join(r.dir, f))
		if err != nil {
			s += f + ":missing;"
			continue
		}
		s += fmt.Sprintf("%s:%d:%d;", f, fi.Size(), fi.ModTime().UnixNano())
	}
	return s
}

func (r *Reloader) load() error {
	stamp := r.fileStamp()
	b, err := LoadBundle(r.dir)
	if err != nil {
		return err
	}
	b.Trust = r.trust
	cfg, err := b.ServerConfig()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cfg, r.bundle, r.stamp = cfg, b, stamp
	if r.live != nil && r.live.Identity.SameNode(b.Identity) {
		r.live.live.Store(&b.Cert)
	}
	r.mu.Unlock()
	return nil
}

// Bundle returns the current bundle.
func (r *Reloader) Bundle() *Bundle {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bundle
}

// TLSConfig returns a server config that resolves to the current bundle on
// every handshake.
func (r *Reloader) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h2"},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			r.mu.RLock()
			defer r.mu.RUnlock()
			return r.cfg, nil
		},
	}
}

// Check reloads the bundle if its files changed; it reports whether a new
// bundle was installed.
func (r *Reloader) Check() (bool, error) {
	r.mu.RLock()
	same := r.stamp == r.fileStamp()
	old := r.bundle
	r.mu.RUnlock()
	if same {
		return false, nil
	}
	if err := r.load(); err != nil {
		// Mid-copy states are common (three files): retry on the next tick.
		return false, err
	}
	nb := r.Bundle()
	r.log.Info("TLS bundle reloaded", "identity", nb.Identity.String(), "not_after", nb.NotAfter)
	if nb.Identity.Name != old.Identity.Name {
		r.log.Warn("worker certificate name changed; Node.Info keeps reporting the old name until longarmd restarts",
			"old", old.Identity.Name, "new", nb.Identity.Name)
	}
	return true, nil
}

// Watch polls the bundle every interval until ctx is done.
func (r *Reloader) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastErr string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.Check(); err != nil {
				if err.Error() != lastErr {
					r.log.Warn("TLS bundle changed but cannot be loaded; keeping the previous one", "err", err)
				}
				lastErr = err.Error()
			} else {
				lastErr = ""
			}
		}
	}
}
