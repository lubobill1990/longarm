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

	"github.com/lubobill1990/longarm/internal/hub/store"
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
	// BootID is new on every run: the deploy scripts wait for a new one
	// after a restart, so they never print a previous start's values.
	BootID string
}

// countNodes is the number of nodes in the database at path (0 when new).
func countNodes(path string) (int, error) {
	st, err := openStoreAt(path)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	nodes, err := st.Nodes()
	return len(nodes), err
}

// Meta keys of the CA copy in the database (Config.CAInDB).
const (
	metaCACert = "ca_cert_pem"
	metaCAKey  = "ca_key_pem"
)

// restoreCAFromDB writes the database's copy of the CA into caDir (false:
// the database has none).
func restoreCAFromDB(cfg *Config, caDir string) (bool, error) {
	st, err := openStoreAt(cfg.DatabasePath())
	if err != nil {
		return false, err
	}
	defer st.Close()
	crt, err := st.Meta(metaCACert)
	if err != nil {
		return false, err
	}
	key, err := st.Meta(metaCAKey)
	if err != nil || crt == "" || key == "" {
		return false, err
	}
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		return false, err
	}
	if err := writeFileAtomic(filepath.Join(caDir, pki.CACertFile), []byte(crt), 0o644); err != nil {
		return false, err
	}
	if err := writeFileAtomic(filepath.Join(caDir, pki.CAKeyFile), []byte(key), 0o600); err != nil {
		return false, err
	}
	if _, err := pki.LoadCA(caDir); err != nil {
		return false, fmt.Errorf("the CA restored from the database: %w", err)
	}
	return true, nil
}

// saveCAToDB stores the CA in caDir in the database, unless it has it
// already. The files win: a CA imported (longarm-hub ca import) replaces
// the copy on the next bootstrap.
func saveCAToDB(cfg *Config, caDir string) error {
	crt, err := os.ReadFile(filepath.Join(caDir, pki.CACertFile))
	if err != nil {
		return err
	}
	key, err := os.ReadFile(filepath.Join(caDir, pki.CAKeyFile))
	if err != nil {
		return err
	}
	st, err := openStoreAt(cfg.DatabasePath())
	if err != nil {
		return err
	}
	defer st.Close()
	oldCrt, err := st.Meta(metaCACert)
	if err != nil {
		return err
	}
	oldKey, err := st.Meta(metaCAKey)
	if err != nil {
		return err
	}
	if oldCrt == string(crt) && oldKey == string(key) {
		return nil
	}
	if err := st.SetMeta(metaCACert, string(crt)); err != nil {
		return err
	}
	return st.SetMeta(metaCAKey, string(key))
}

// openStoreAt opens the database at path, creating its directory.
func openStoreAt(path string) (*store.Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return store.Open(path)
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
	res := &BootstrapResult{HubAddress: cfg.PublicAddr, BootID: randomToken()[:16]}
	caDir := filepath.Join(cfg.DataDir, CADir)
	_, crtErr := os.Stat(filepath.Join(caDir, pki.CACertFile))
	_, keyErr := os.Stat(filepath.Join(caDir, pki.CAKeyFile))
	for _, err := range []error{crtErr, keyErr} {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if cfg.CAInDB && (crtErr != nil || keyErr != nil) {
		restored, err := restoreCAFromDB(cfg, caDir)
		if err != nil {
			return nil, err
		}
		if restored {
			crtErr, keyErr = nil, nil
			log.Info("bootstrap: restored the CA from the database", "dir", caDir)
		}
	}
	if crtErr != nil || keyErr != nil {
		// A CA is only made for a hub that never issued anything. A database
		// with nodes next to a missing CA means the data dir was lost or is
		// the wrong one: a new CA would silently cut off every node.
		n, err := countNodes(cfg.DatabasePath())
		if err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, fmt.Errorf("the database has %d nodes but %s has no complete CA (%s + %s): restore the CA (longarm-hub ca import) instead of creating a new one",
				n, caDir, pki.CACertFile, pki.CAKeyFile)
		}
		if crtErr == nil || keyErr == nil {
			// half a CA and nothing issued: an interrupted first start
			old := fmt.Sprintf("%s.incomplete-%d", caDir, time.Now().Unix())
			if err := os.Rename(caDir, old); err != nil {
				return nil, err
			}
			log.Warn("bootstrap: moved an incomplete CA aside", "to", old)
		}
		if _, err := InitCA(cfg.DataDir); err != nil {
			return nil, fmt.Errorf("init CA: %w", err)
		}
		res.NewCA = true
		log.Info("bootstrap: created a new CA", "dir", caDir)
	}
	if cfg.CAInDB {
		if err := saveCAToDB(cfg, caDir); err != nil {
			return nil, err
		}
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
	for _, kv := range r.pairs(true) {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
	}
	return b.Bytes()
}

// LogLine is the single-line form (BootstrapLogPrefix key=value ...) for
// the container log. It never has the join code: an Azure container's log
// needs less to read (Reader) than the file share (the storage key).
func (r *BootstrapResult) LogLine() string {
	parts := []string{BootstrapLogPrefix}
	for _, kv := range r.pairs(false) {
		parts = append(parts, kv[0]+"="+kv[1])
	}
	return strings.Join(parts, " ")
}

func (r *BootstrapResult) pairs(withCode bool) [][2]string {
	exp := ""
	if !r.AdminCodeExpires.IsZero() {
		exp = r.AdminCodeExpires.UTC().Format(time.RFC3339)
	}
	kv := [][2]string{
		{"boot_id", r.BootID},
		{"ca_fingerprint", r.Fingerprint},
		{"hub_address", r.HubAddress},
		{"admin_joined", fmt.Sprint(r.AdminJoined)},
	}
	if withCode {
		kv = append(kv, [2]string{"admin_join_code", r.AdminCode})
	}
	return append(kv, [2]string{"admin_join_code_expires", exp})
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
