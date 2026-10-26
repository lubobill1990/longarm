// Package trust keeps a node's end-to-end trust state on disk (pki/trust.go,
// docs/security.md "End-to-end trust"): its node key, its endorsement, the
// user key on devices that hold one, and the policy of whose endorsements
// count. Everything here is local and owner-controlled: nothing the hub
// sends can change it.
//
//	<dir>/node.key        Ed25519 node key (created on first use, 0600)
//	<dir>/endorsement     the node's endorsement (a longarm-endorsement line)
//	<dir>/user.key        a user's Ed25519 key (devices only, 0600)
//	<dir>/trust.toml      the policy (users whose endorsements count, ...)
//	<dir>/renew           a request to renew the certificate now (new endorsement)
package trust

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
)

// Files in a trust directory.
const (
	NodeKeyFile     = "node.key"
	EndorsementFile = "endorsement"
	UserKeyFile     = "user.key"
	PolicyFile      = "trust.toml"
	RenewFile       = "renew"
)

// Store is a trust directory: <data>/trust (longarmd), <config>/trust
// (longarm).
type Store struct{ Dir string }

func (s Store) path(f string) string { return filepath.Join(s.Dir, f) }

func (s Store) write(f string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	tmp := s.path(f) + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return platform.ReplaceFile(tmp, s.path(f))
}

// ---------------------------------------------------------------- keys

func readKey(path, pemType string) (ed25519.PrivateKey, map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != pemType {
		return nil, nil, fmt.Errorf("%s: no %s PEM block", path, pemType)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("%s: not an Ed25519 key", path)
	}
	return ek, blk.Headers, nil
}

func encodeKey(k ed25519.PrivateKey, pemType string, headers map[string]string) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemType, Headers: headers, Bytes: der}), nil
}

// NodeKey returns the node key, creating it on first use.
func (s Store) NodeKey() (ed25519.PrivateKey, error) {
	k, _, err := readKey(s.path(NodeKeyFile), "LONGARM NODE KEY")
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return k, err
	}
	_, k, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := encodeKey(k, "LONGARM NODE KEY", nil)
	if err != nil {
		return nil, err
	}
	return k, s.write(NodeKeyFile, b, 0o600)
}

// UserKey returns the user key held here and its user (os.ErrNotExist:
// none).
func (s Store) UserKey() (ed25519.PrivateKey, string, error) {
	k, h, err := readKey(s.path(UserKeyFile), "LONGARM USER KEY")
	if err != nil {
		return nil, "", err
	}
	if !pki.ValidUser(h["User"]) {
		return nil, "", fmt.Errorf("%s: no valid User header", s.path(UserKeyFile))
	}
	return k, h["User"], nil
}

// CreateUserKey makes a new user key for user (refusing to replace one).
func (s Store) CreateUserKey(user string) (ed25519.PrivateKey, error) {
	if !pki.ValidUser(user) {
		return nil, fmt.Errorf("invalid user %q", user)
	}
	if _, err := os.Stat(s.path(UserKeyFile)); err == nil {
		return nil, fmt.Errorf("%s exists already", s.path(UserKeyFile))
	}
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := encodeKey(k, "LONGARM USER KEY", map[string]string{"User": user})
	if err != nil {
		return nil, err
	}
	return k, s.write(UserKeyFile, b, 0o600)
}

// ExportUserKey returns the user key file (PEM) to copy to another device.
func (s Store) ExportUserKey() ([]byte, error) {
	if _, _, err := s.UserKey(); err != nil {
		return nil, err
	}
	return os.ReadFile(s.path(UserKeyFile))
}

// ImportUserKey installs an exported user key (refusing to replace a
// different one).
func (s Store) ImportUserKey(b []byte) (string, error) {
	tmp, err := os.CreateTemp("", "longarm-user-key-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	tmp.Write(b)
	tmp.Close()
	k, h, err := readKey(tmp.Name(), "LONGARM USER KEY")
	if err != nil {
		return "", errors.New("not a longarm user key (longarm trust export-key)")
	}
	user := h["User"]
	if !pki.ValidUser(user) {
		return "", errors.New("the user key names no valid user")
	}
	if old, _, err := s.UserKey(); err == nil {
		if old.Equal(k) {
			return user, nil
		}
		return "", fmt.Errorf("%s holds another user key", s.path(UserKeyFile))
	}
	return user, s.write(UserKeyFile, b, 0o600)
}

// ---------------------------------------------------------------- endorsement

// Endorsement returns the node's endorsement (nil: not endorsed).
func (s Store) Endorsement() (*pki.Endorsement, error) {
	b, err := os.ReadFile(s.path(EndorsementFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return pki.ParseEndorsement(string(b))
}

// Request is the endorsement request of this node.
func (s Store) Request(kind, name, user string) (pki.EndorseRequest, error) {
	k, err := s.NodeKey()
	if err != nil {
		return pki.EndorseRequest{}, err
	}
	return pki.EndorseRequest{Kind: kind, Name: name, User: user, NodeKey: k.Public().(ed25519.PublicKey)}, nil
}

// SetEndorsement stores e after checking it is for this node as kind and
// name, and asks for a certificate renewal (the certificate must carry it).
func (s Store) SetEndorsement(e *pki.Endorsement, kind, name string) error {
	k, err := s.NodeKey()
	if err != nil {
		return err
	}
	if !k.Public().(ed25519.PublicKey).Equal(ed25519.PublicKey(e.NodeKey)) {
		return errors.New("the endorsement is for another node (endorse this node's request)")
	}
	if e.Kind != kind || e.Name != name {
		return fmt.Errorf("the endorsement is for %s %q, this is %s %q", e.Kind, e.Name, kind, name)
	}
	if err := e.Verify(); err != nil {
		return err
	}
	if err := s.write(EndorsementFile, []byte(e.String()+"\n"), 0o644); err != nil {
		return err
	}
	return s.RequestRenewal()
}

// CSRTrust is the material for the node's next CSR.
func (s Store) CSRTrust() (*pki.CSRTrust, error) {
	k, err := s.NodeKey()
	if err != nil {
		return nil, err
	}
	e, err := s.Endorsement()
	if err != nil {
		return nil, err
	}
	return &pki.CSRTrust{NodeKey: k, Endorsement: e}, nil
}

// RequestRenewal asks the running daemon to renew the certificate now.
func (s Store) RequestRenewal() error { return s.write(RenewFile, nil, 0o644) }

// RenewalRequested reports a pending RequestRenewal.
func (s Store) RenewalRequested() bool {
	_, err := os.Stat(s.path(RenewFile))
	return err == nil
}

// RenewalDone clears the request.
func (s Store) RenewalDone() { os.Remove(s.path(RenewFile)) }

// ---------------------------------------------------------------- policy

// PolicyFileData is trust.toml.
type PolicyFileData struct {
	// Users: user name -> user key (base64 or a longarm-user-key line).
	// On a worker: whose endorsed devices may use it (non-empty = required).
	// On a client: whose endorsed workers it accepts (with require_workers).
	Users map[string]string `toml:"users"`
	// DenyNodes: node keys (base64) refused even when endorsed.
	DenyNodes []string `toml:"deny_nodes"`
	// RequireWorkers (clients): only talk to workers endorsed by a user of
	// Users or by this device's own user key.
	RequireWorkers bool `toml:"require_workers"`
}

// LoadPolicyFile reads trust.toml (missing = empty).
func (s Store) LoadPolicyFile() (*PolicyFileData, error) {
	var f PolicyFileData
	if _, err := toml.DecodeFile(s.path(PolicyFile), &f); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", s.path(PolicyFile), err)
	}
	if f.Users == nil {
		f.Users = map[string]string{}
	}
	return &f, nil
}

// SavePolicyFile writes trust.toml.
func (s Store) SavePolicyFile(f *PolicyFileData) error {
	var b strings.Builder
	b.WriteString("# longarm end-to-end trust (docs/security.md). Edited by `longarm trust` / `longarmd trust`.\n")
	if f.RequireWorkers {
		b.WriteString("require_workers = true\n")
	}
	if len(f.DenyNodes) > 0 {
		b.WriteString("deny_nodes = [")
		for i, d := range f.DenyNodes {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", d)
		}
		b.WriteString("]\n")
	}
	b.WriteString("\n[users]\n")
	names := make([]string, 0, len(f.Users))
	for n := range f.Users {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "%s = %q\n", n, f.Users[n])
	}
	return s.write(PolicyFile, []byte(b.String()), 0o644)
}

func parseUserKey(user, v string) (ed25519.PublicKey, error) {
	if l, err := pki.ParseUserKeyLine(v); err == nil {
		if l.User != user {
			return nil, fmt.Errorf("users.%s: the key line is for user %q", user, l.User)
		}
		return l.Key, nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("users.%s: not an Ed25519 public key (base64) or a longarm-user-key line", user)
	}
	return b, nil
}

// Policy is the effective policy. forClient: the device's own user key
// counts too, and the policy is only active with require_workers.
func (s Store) Policy(forClient bool) (*pki.TrustPolicy, error) {
	f, err := s.LoadPolicyFile()
	if err != nil {
		return nil, err
	}
	p := &pki.TrustPolicy{Users: map[string]ed25519.PublicKey{}}
	for u, v := range f.Users {
		if !pki.ValidUser(u) {
			return nil, fmt.Errorf("%s: invalid user %q", s.path(PolicyFile), u)
		}
		k, err := parseUserKey(u, v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.path(PolicyFile), err)
		}
		p.Users[u] = k
	}
	for _, d := range f.DenyNodes {
		b, err := base64.StdEncoding.DecodeString(d)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%s: deny_nodes: %q is not a node key (base64)", s.path(PolicyFile), d)
		}
		p.DenyNodes = append(p.DenyNodes, b)
	}
	if forClient {
		if !f.RequireWorkers {
			return nil, nil
		}
		if k, user, err := s.UserKey(); err == nil {
			if old, ok := p.Users[user]; ok && !old.Equal(k.Public()) {
				return nil, fmt.Errorf("%s: users.%s differs from this device's own user key", s.path(PolicyFile), user)
			}
			p.Users[user] = k.Public().(ed25519.PublicKey)
		}
		if len(p.Users) == 0 {
			return nil, fmt.Errorf("%s: require_workers without a user key or users", s.path(PolicyFile))
		}
	}
	return p, nil
}

// AddUser trusts a user key (replacing the user's previous key).
func (s Store) AddUser(l pki.UserKeyLine) error {
	f, err := s.LoadPolicyFile()
	if err != nil {
		return err
	}
	f.Users[l.User] = base64.StdEncoding.EncodeToString(l.Key)
	return s.SavePolicyFile(f)
}

// RemoveUser stops trusting user.
func (s Store) RemoveUser(user string) error {
	f, err := s.LoadPolicyFile()
	if err != nil {
		return err
	}
	if _, ok := f.Users[user]; !ok {
		return fmt.Errorf("user %q is not trusted here", user)
	}
	delete(f.Users, user)
	return s.SavePolicyFile(f)
}

// DenyNode refuses a node key (base64) even when endorsed.
func (s Store) DenyNode(key string) error {
	b, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return errors.New("not a node key (base64)")
	}
	f, err := s.LoadPolicyFile()
	if err != nil {
		return err
	}
	if !slices.Contains(f.DenyNodes, key) {
		f.DenyNodes = append(f.DenyNodes, key)
	}
	return s.SavePolicyFile(f)
}

// SetRequireWorkers turns the client's worker check on or off.
func (s Store) SetRequireWorkers(on bool) error {
	f, err := s.LoadPolicyFile()
	if err != nil {
		return err
	}
	old := *f
	f.RequireWorkers = on
	if err := s.SavePolicyFile(f); err != nil {
		return err
	}
	if _, err := s.Policy(true); err != nil {
		s.SavePolicyFile(&old)
		return err
	}
	return nil
}

// Source loads the policy into a pki.TrustSource and keeps it current
// (Watch).
func (s Store) Source(forClient bool) (*pki.TrustSource, error) {
	p, err := s.Policy(forClient)
	if err != nil {
		return nil, err
	}
	src := &pki.TrustSource{}
	src.Store(p)
	return src, nil
}

// Watch reloads the policy into src when trust.toml (or the user key)
// changes; a broken file keeps the previous policy and is logged.
func (s Store) Watch(ctx context.Context, src *pki.TrustSource, forClient bool, every time.Duration, log *slog.Logger) {
	stamp := s.stamp()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if st := s.stamp(); st != stamp {
			p, err := s.Policy(forClient)
			if err != nil {
				log.Warn("trust policy not reloaded; keeping the previous one", "err", err)
				continue
			}
			stamp = st
			src.Store(p)
			log.Info("trust policy reloaded", "summary", Summary(p))
		}
	}
}

func (s Store) stamp() string {
	var b strings.Builder
	for _, f := range []string{PolicyFile, UserKeyFile} {
		if fi, err := os.Stat(s.path(f)); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", f, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

// Summary describes a policy for logs.
func Summary(p *pki.TrustPolicy) string {
	if !p.Active() {
		return "off"
	}
	var us []string
	for u, k := range p.Users {
		us = append(us, u+"("+pki.KeyID(k)+")")
	}
	sort.Strings(us)
	return fmt.Sprintf("users %s, %d denied nodes", strings.Join(us, " "), len(p.DenyNodes))
}
