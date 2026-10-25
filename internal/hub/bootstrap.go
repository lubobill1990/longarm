package hub

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// BootstrapFile in the data dir records the first-start state for the
// deploy scripts (deploy/azure): key=value lines, see BootstrapResult.
const BootstrapFile = "bootstrap.env"

// BootstrapLogPrefix starts the one log line Bootstrap prints with the same
// values, for when only the container log is readable.
const BootstrapLogPrefix = "LONGARM_BOOTSTRAP"

// BootstrapResult is what a container's first start produced.
type BootstrapResult struct {
	Fingerprint string // CA fingerprint (for --ca-fingerprint)
	NewCA       bool   // the CA was created by this call
	HubAddress  string // public_addr, if configured
	// AdminCode is a one-time admin join code; empty once an admin has
	// joined (then `longarm admin join-code` or `longarm-hub join-code`).
	AdminCode        string
	AdminCodeExpires time.Time
	AdminJoined      bool
}

// Bootstrap prepares a hub for its first (or any) start without operator
// steps: the CA is created when the data dir has none, and as long as no
// admin has joined there is one valid admin join code (a new one when the
// last expired or the database lost it). The result is also written to
// <data_dir>/bootstrap.env. Safe to run on every start.
func Bootstrap(cfg *Config, codeTTL time.Duration, log *slog.Logger) (*BootstrapResult, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	res := &BootstrapResult{HubAddress: cfg.PublicAddr}
	caDir := filepath.Join(cfg.DataDir, CADir)
	_, crtErr := os.Stat(filepath.Join(caDir, pki.CACertFile))
	_, keyErr := os.Stat(filepath.Join(caDir, pki.CAKeyFile))
	switch {
	case errors.Is(crtErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
		if _, err := InitCA(cfg.DataDir); err != nil {
			return nil, fmt.Errorf("init CA: %w", err)
		}
		res.NewCA = true
		log.Info("bootstrap: created a new CA", "dir", caDir)
	case crtErr != nil || keyErr != nil:
		// half a CA: never overwrite what might be the fleet's only key
		return nil, fmt.Errorf("CA in %s is incomplete (%s and %s must both exist): %v %v", caDir, pki.CACertFile, pki.CAKeyFile, crtErr, keyErr)
	}

	h, err := Open(cfg, log)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	res.Fingerprint = pki.Fingerprint(h.ca.Cert)
	nodes, err := h.store.Nodes()
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.Role == string(pki.RoleAdmin) && n.State == stateApproved {
			res.AdminJoined = true
		}
	}
	path := filepath.Join(cfg.DataDir, BootstrapFile)
	if !res.AdminJoined {
		prev := readKV(path)
		now := time.Now()
		if c := prev["admin_join_code"]; c != "" {
			if _, err := h.store.PeekJoinCode(codeHash(c), now); err == nil {
				res.AdminCode = c
				res.AdminCodeExpires, _ = time.Parse(time.RFC3339, prev["admin_join_code_expires"])
			}
		}
		if res.AdminCode == "" {
			jc, err := h.CreateJoinCode("bootstrap", &pb.CreateJoinCodeRequest{Role: string(pki.RoleAdmin), Ttl: durationpb.New(codeTTL)})
			if err != nil {
				return nil, err
			}
			res.AdminCode, res.AdminCodeExpires = jc.Code, jc.ExpiresAt.AsTime()
		}
	}
	if err := writeFileAtomic(path, res.env(), 0o600); err != nil {
		return nil, err
	}
	return res, nil
}

// env is the bootstrap.env content.
func (r *BootstrapResult) env() []byte {
	var b bytes.Buffer
	b.WriteString("# longarm-hub bootstrap (rewritten on every container start)\n")
	for _, kv := range r.pairs() {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
	}
	return b.Bytes()
}

// LogLine is the single-line form (BootstrapLogPrefix key=value ...).
func (r *BootstrapResult) LogLine() string {
	parts := []string{BootstrapLogPrefix}
	for _, kv := range r.pairs() {
		parts = append(parts, kv[0]+"="+kv[1])
	}
	return strings.Join(parts, " ")
}

func (r *BootstrapResult) pairs() [][2]string {
	exp := ""
	if !r.AdminCodeExpires.IsZero() {
		exp = r.AdminCodeExpires.UTC().Format(time.RFC3339)
	}
	return [][2]string{
		{"ca_fingerprint", r.Fingerprint},
		{"hub_address", r.HubAddress},
		{"admin_joined", fmt.Sprint(r.AdminJoined)},
		{"admin_join_code", r.AdminCode},
		{"admin_join_code_expires", exp},
	}
}

// readKV reads key=value lines (missing file = empty).
func readKV(path string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// writeFileAtomic writes via a temp file + rename (also on SMB shares).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ConfigTemplate is the hub.toml a container's first start writes when
// LONGARM_HUB_CONFIG names a missing file: everything container-specific
// comes from the environment (config_env.go), the file is for the operator's
// own settings (edit it on the share, restart the container).
const ConfigTemplate = `# longarm-hub configuration (written by the container's first start; edit and
# restart the container). The deployment sets these from the environment,
# which wins over this file: LONGARM_HUB_ADVERTISE (public_addr + hosts),
# LONGARM_HUB_PUBLIC_URL, LONGARM_HUB_HTTP, LONGARM_HUB_LISTEN, LONGARM_HUB_DB,
# LONGARM_HUB_DATA_DIR. All settings: deploy/hub/hub.toml.example.

auto_approve = false               # joining workers wait for ` + "`longarm admin approve <name>`" + `

[certs]
worker_ttl = "2160h"               # 90 days
client_ttl = "720h"                # 30 days (client and admin)

# [tailnet]                        # Tailscale/Headscale leg: deploy/hub/README.md
# provider = "tailscale"           # secret: env LONGARM_HUB_TAILSCALE_OAUTH_SECRET
`

// WriteConfigTemplate writes ConfigTemplate to path unless it exists.
func WriteConfigTemplate(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	return true, writeFileAtomic(path, []byte(ConfigTemplate), 0o600)
}
