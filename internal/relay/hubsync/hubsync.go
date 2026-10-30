// Package hubsync ties a standalone longarm-relay to a hub (longarm-relay
// --hub): the relay registers its public endpoints with the hub (which
// advertises them to nodes as part of its relay set) and receives the policy
// it enforces on its own - the revocation list and the worker pairs it may
// relay between (port links). Nothing per session goes to the hub: the relay
// authenticates nodes by their CA certificates and checks the cached list,
// so it keeps relaying while the hub restarts or is down.
//
// The policy is kept on disk (StateDir): a relay that restarts while the hub
// is away starts with the last list. A relay that never had one refuses
// sessions until the hub answers (fail closed: it can't know who is revoked).
// Worker pairs expire RelayPolicy.ttl after the last contact with the hub;
// the revocation list never expires (it only ever restricts).
package hubsync

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/relay"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Files in Config.StateDir.
const (
	RevocationsFile = "revocations.json"
	PolicyFile      = "relay-policy.json"
)

// DefaultTTL applies when the hub sends none.
const DefaultTTL = 24 * time.Hour

// Timing knobs (variables for tests).
var (
	backoffMin = time.Second
	backoffMax = 30 * time.Second
	// silence: no policy for this long = the stream is dead (the hub sends
	// one at least every minute)
	silence = 3 * time.Minute
)

// Config of a Sync.
type Config struct {
	Hub      string      // the hub's "host:port"
	Bundle   *pki.Bundle // the relay's live bundle (pki.Bundle.Live): renewals swap its certificate
	TLSDir   string      // renewed certificates are written here ("" = kept in memory only)
	StateDir string      // policy cache ("" = none)
	UDP, TCP string      // the public endpoints registered with the hub
	Server   *relay.Server
	Logger   *slog.Logger
	Version  string
}

// Sync is the relay's hub subscription.
type Sync struct {
	cfg     Config
	log     *slog.Logger
	revoked *pki.RevocationList

	mu        sync.Mutex
	ready     bool      // a policy is known (live or cached)
	contact   time.Time // last policy from the hub
	ttl       time.Duration
	pairs     [][2]string
	pairsOn   bool // pairs are installed in the server
	connected bool
	lastErr   string
	renewTry  time.Time
	changed   chan struct{}
}

type policyFile struct {
	Received time.Time   `json:"received"`
	TTLSec   int64       `json:"ttl_s"`
	Pairs    [][2]string `json:"pairs"`
}

// New loads the cached policy (if any) and installs it in cfg.Server.
func New(cfg Config) (*Sync, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Server == nil || cfg.Bundle == nil || cfg.Hub == "" {
		return nil, errors.New("hubsync: Hub, Bundle and Server are required")
	}
	s := &Sync{cfg: cfg, log: cfg.Logger, ttl: DefaultTTL, changed: make(chan struct{})}
	revPath := ""
	if cfg.StateDir != "" {
		if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
			return nil, err
		}
		revPath = filepath.Join(cfg.StateDir, RevocationsFile)
	}
	rl, err := pki.LoadRevocationList(revPath)
	if err != nil {
		cfg.Logger.Warn("relay: cached revocation list unreadable; waiting for the hub", "err", err)
		rl, _ = pki.LoadRevocationList("")
	}
	s.revoked = rl
	if cfg.StateDir != "" {
		if b, err := os.ReadFile(filepath.Join(cfg.StateDir, PolicyFile)); err == nil {
			var pf policyFile
			if json.Unmarshal(b, &pf) == nil && !pf.Received.IsZero() { // the list (version 0: nothing revoked yet) is saved first
				s.ready, s.contact, s.pairs = true, pf.Received, pf.Pairs
				if pf.TTLSec > 0 {
					s.ttl = time.Duration(pf.TTLSec) * time.Second
				}
				cfg.Logger.Info("relay: using the cached hub policy until the hub answers", "received", pf.Received,
					"revoked", rl.Len(), "worker_pairs", len(pf.Pairs))
			}
		}
	}
	s.applyPairs(time.Now())
	return s, nil
}

// VerifyPeerCertificate checks a verified chain (tests; Install uses
// VerifyConnection): no session before a revocation list is known, none
// with a revoked certificate.
func (s *Sync) VerifyPeerCertificate(raw [][]byte, chains [][]*x509.Certificate) error {
	for _, ch := range chains {
		if len(ch) > 0 {
			if err := s.Admit(ch[0]); err != nil {
				return err
			}
		}
	}
	return nil
}

// Admit is the relay's admission check of a node certificate.
func (s *Sync) Admit(c *x509.Certificate) error {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		return errors.New("the relay has no revocation list from its hub yet")
	}
	if c == nil {
		return errors.New("no client certificate")
	}
	if s.revoked.Revoked(c) {
		return fmt.Errorf("certificate %s is revoked", pki.Serial(c))
	}
	return nil
}

// Install puts the checks into the relay: in the TLS config through
// VerifyConnection, which also runs on resumed handshakes (unlike
// VerifyPeerCertificate), with session tickets off, and again when a
// session registers (Server.Admit), atomically with the sweep of a new
// revocation list.
func (s *Sync) Install(tc *tls.Config) {
	tc.SessionTicketsDisabled = true
	tc.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("no client certificate")
		}
		return s.Admit(cs.PeerCertificates[0])
	}
	s.cfg.Server.Admit = s.Admit
}

// Status reports the subscription's state (logs, /healthz).
type Status struct {
	Ready       bool      `json:"ready"`
	Connected   bool      `json:"connected"`
	LastContact time.Time `json:"last_contact"`
	Revoked     int       `json:"revoked"`
	WorkerPairs int       `json:"worker_pairs"`
	PairsActive bool      `json:"worker_pairs_active"`
	Error       string    `json:"error,omitempty"`
}

func (s *Sync) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Ready: s.ready, Connected: s.connected, LastContact: s.contact, Revoked: s.revoked.Len(),
		WorkerPairs: len(s.pairs), PairsActive: s.pairsOn, Error: s.lastErr}
}

// Changed returns a channel closed at the next policy (tests).
func (s *Sync) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// applyPairs installs the worker pairs, or none once the policy is older
// than its TTL.
func (s *Sync) applyPairs(now time.Time) {
	s.mu.Lock()
	on := s.ready && now.Sub(s.contact) < s.ttl
	pairs := s.pairs
	was := s.pairsOn
	s.pairsOn = on
	s.mu.Unlock()
	if on {
		s.cfg.Server.SetWorkerPairs(pairs)
	} else {
		s.cfg.Server.SetWorkerPairs(nil)
		if was {
			s.log.Warn("relay: no word from the hub for longer than the policy TTL: no longer relaying between workers (port links) "+
				"until it answers; client <-> worker sessions continue", "ttl", s.ttl)
		}
	}
}

// Run keeps the subscription up until ctx ends.
func (s *Sync) Run(ctx context.Context) {
	wait := backoffMin
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				s.applyPairs(now)
			}
		}
	}()
	for ctx.Err() == nil {
		got, err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.connected = false
		if err != nil {
			s.lastErr = err.Error()
		}
		s.mu.Unlock()
		if got {
			wait = backoffMin
		}
		s.log.Warn("relay: hub subscription ended; relaying continues with the last policy", "hub", s.cfg.Hub, "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, backoffMax)
	}
}

// session runs one RelaySync stream; got = at least one policy arrived.
func (s *Sync) session(ctx context.Context) (got bool, err error) {
	conn, err := hubclient.Dial(s.cfg.Hub, s.cfg.Bundle)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := conn.Node().RelaySync(sctx, &pb.RelaySyncRequest{Udp: s.cfg.UDP, Tcp: s.cfg.TCP, Version: s.cfg.Version})
	if err != nil {
		return false, err
	}
	msgs := make(chan *pb.RelayPolicy)
	errc := make(chan error, 1)
	go func() {
		for {
			p, err := st.Recv()
			if err != nil {
				errc <- err
				return
			}
			select {
			case msgs <- p:
			case <-sctx.Done():
				return
			}
		}
	}()
	timer := time.NewTimer(silence)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return got, ctx.Err()
		case err := <-errc:
			return got, err
		case <-timer.C:
			return got, fmt.Errorf("no policy from the hub for %s", silence)
		case p := <-msgs:
			if !got {
				s.log.Info("relay: subscribed to the hub", "hub", s.cfg.Hub, "udp", s.cfg.UDP, "tcp", s.cfg.TCP)
			}
			got = true
			timer.Reset(silence)
			s.apply(p)
			s.maybeRenew(sctx, conn, p)
		}
	}
}

// apply installs a policy from the hub.
func (s *Sync) apply(p *pb.RelayPolicy) {
	now := time.Now()
	oldVersion := s.revoked.Version()
	// serials, node IDs and legacy names: the same list workers enforce
	if err := hubclient.ApplyRevocations(s.revoked, p.GetRevocations()); err != nil {
		s.log.Warn("relay: save revocation list", "err", err)
	}
	// every policy sweeps (a session that slipped in is closed at the latest
	// with the next one); registration checks the list too (Server.Admit)
	n := s.cfg.Server.KickCerts(s.revoked.Revoked, "revoked")
	if v := s.revoked.Version(); v != oldVersion || n > 0 {
		s.log.Info("relay: revocation list updated", "version", v, "revoked", s.revoked.Len(), "sessions_closed", n)
	}
	var pairs [][2]string
	for _, wp := range p.WorkerPairs {
		if wp.A != "" && wp.B != "" && wp.A != wp.B {
			pairs = append(pairs, [2]string{wp.A, wp.B})
		}
	}
	ttl := p.GetTtl().AsDuration()
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	s.mu.Lock()
	s.ready, s.connected, s.contact, s.ttl, s.pairs, s.lastErr = true, true, now, ttl, pairs, ""
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	s.applyPairs(now)
	if s.cfg.StateDir != "" {
		b, _ := json.Marshal(policyFile{Received: now, TTLSec: int64(ttl / time.Second), Pairs: pairs})
		path := filepath.Join(s.cfg.StateDir, PolicyFile)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil || platform.ReplaceFile(tmp, path) != nil {
			os.Remove(tmp)
			s.log.Warn("relay: save the hub policy", "path", path, "err", err)
		}
	}
}

// maybeRenew renews the relay certificate through the hub when it expires
// within the policy's renew_before (at most one attempt per hour).
func (s *Sync) maybeRenew(ctx context.Context, conn *hubclient.Conn, p *pb.RelayPolicy) {
	rb := p.GetRenewBefore().AsDuration()
	leaf := s.cfg.Bundle.Leaf()
	if !hubclient.NeedsRenewal(leaf, rb) {
		return
	}
	s.mu.Lock()
	if time.Since(s.renewTry) < time.Hour {
		s.mu.Unlock()
		return
	}
	s.renewTry = time.Now()
	s.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	certPEM, keyPEM, caPEM, err := hubclient.Renew(rctx, conn, s.cfg.Bundle.Identity.Name, nil)
	if err != nil {
		s.log.Warn("relay: certificate renewal failed", "err", err, "not_after", leaf.NotAfter)
		return
	}
	nb, err := pki.ParseBundle(caPEM, certPEM, keyPEM)
	if err != nil {
		s.log.Warn("relay: renewed certificate unusable", "err", err)
		return
	}
	if s.cfg.TLSDir != "" {
		if err := pki.WriteBundleAtomic(s.cfg.TLSDir, caPEM, certPEM, keyPEM); err != nil {
			s.log.Warn("relay: write renewed certificate (kept in memory)", "dir", s.cfg.TLSDir, "err", err)
		}
	}
	if err := s.cfg.Bundle.Update(nb); err != nil {
		s.log.Warn("relay: install renewed certificate", "err", err)
		return
	}
	s.log.Info("relay: certificate renewed", "not_after", nb.Leaf().NotAfter)
}
