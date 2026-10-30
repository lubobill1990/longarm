// Package store is longarm-hub's persistence: one SQLite database (pure Go
// driver modernc.org/sqlite, no cgo) in WAL mode, so Litestream can stream
// it to object storage (DESIGN §3.3). Schema changes are numbered migrations
// recorded in the meta table; a migration may have a Go step too
// (goMigrations), run in the same transaction after its SQL.
//
// The hub keeps its working state in memory and writes through to the store;
// the store is read completely at start. Jobs are a cache of worker reports
// (workers are the source of truth) and are rebuilt when workers reconnect.
package store

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers "sqlite"
)

// Store is the hub database.
type Store struct {
	db *sql.DB
	// MigrationNotes says what migrations of this Open changed beyond the
	// schema (renamed nodes ...), for the hub's log.
	MigrationNotes []string
	// OnNodePut, if set, is called after a node record was saved (the hub's
	// lock-free name directory for event projection).
	OnNodePut func(*Node)
}

// ErrNameTaken: a non-revoked node has the name already (PutNode).
var ErrNameTaken = errors.New("name taken by another active node")

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

var migrations = []string{
	// 1: initial schema
	`CREATE TABLE nodes (
		name TEXT PRIMARY KEY,
		role TEXT NOT NULL,
		state TEXT NOT NULL,
		data TEXT NOT NULL
	);
	CREATE TABLE certs (
		serial TEXT PRIMARY KEY,
		node TEXT NOT NULL,
		not_after INTEGER NOT NULL,
		issued_at INTEGER NOT NULL,
		revoked_at INTEGER
	);
	CREATE INDEX certs_node ON certs(node);
	CREATE TABLE join_codes (
		id TEXT PRIMARY KEY,
		hash TEXT NOT NULL UNIQUE,
		data TEXT NOT NULL,
		expires_at INTEGER NOT NULL,
		used_at INTEGER
	);
	CREATE TABLE jobs (
		worker TEXT NOT NULL,
		id TEXT NOT NULL,
		state INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		data BLOB NOT NULL,
		PRIMARY KEY (worker, id)
	);
	CREATE TABLE audit (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		node TEXT NOT NULL,
		time INTEGER NOT NULL,
		who TEXT NOT NULL,
		method TEXT NOT NULL,
		line TEXT NOT NULL
	);
	CREATE INDEX audit_node_time ON audit(node, time);
	CREATE TABLE users (
		name TEXT PRIMARY KEY,
		data TEXT NOT NULL
	);
	CREATE TABLE web_sessions (
		hash TEXT PRIMARY KEY,
		user TEXT NOT NULL,
		expires_at INTEGER NOT NULL
	);
	CREATE TABLE profiles (
		name TEXT PRIMARY KEY,
		data TEXT NOT NULL
	);
	CREATE TABLE releases (
		version TEXT NOT NULL,
		file TEXT NOT NULL,
		os TEXT NOT NULL,
		arch TEXT NOT NULL,
		sha256 TEXT NOT NULL,
		size INTEGER NOT NULL,
		added_at INTEGER NOT NULL,
		PRIMARY KEY (version, file)
	);`,
	// 2: release channels, package kinds and minisign signatures (P2)
	`ALTER TABLE releases ADD COLUMN channel TEXT NOT NULL DEFAULT 'stable';
	ALTER TABLE releases ADD COLUMN kind TEXT NOT NULL DEFAULT 'binary';
	ALTER TABLE releases ADD COLUMN sig BLOB;`,
	// 3: multi-tenancy: Longarm users (teams, quotas)
	`CREATE TABLE tenants (
		name TEXT PRIMARY KEY,
		data TEXT NOT NULL
	);`,
	// 4: port links between workers (protojson PortLink without status)
	`CREATE TABLE port_links (
		id TEXT PRIMARY KEY,
		owner TEXT NOT NULL,
		data TEXT NOT NULL
	);`,
	// 5: audit attribution, fixed when a record is written: the Longarm
	// user who acted and the user the record is about (the owner of the
	// worker it came from, of the link, the user changed ...). Reads are
	// authorized by these, never by today's node ownership. attributed = 0:
	// a record from before (only its own "user" field counts).
	`ALTER TABLE audit ADD COLUMN actor_user TEXT NOT NULL DEFAULT '';
	ALTER TABLE audit ADD COLUMN subject_user TEXT NOT NULL DEFAULT '';
	ALTER TABLE audit ADD COLUMN attributed INTEGER NOT NULL DEFAULT 0;`,
	// 6: node IDs (docs/security.md "Node identity"): nodes are keyed by an
	// immutable ID, names are display names unique among non-revoked
	// nodes; certificates, audit records, cached jobs and port links refer
	// to the ID. The rows are moved by migrateNodeIDs.
	`CREATE TABLE nodes_v6 (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		role TEXT NOT NULL,
		state TEXT NOT NULL,
		data TEXT NOT NULL
	);
	ALTER TABLE certs ADD COLUMN node_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE certs ADD COLUMN has_id INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE audit ADD COLUMN node_id TEXT NOT NULL DEFAULT '';
	CREATE TABLE revoked_names (
		role TEXT NOT NULL,
		name TEXT NOT NULL,
		not_after INTEGER NOT NULL,
		PRIMARY KEY (role, name)
	);`,
}

// goMigrations are Go steps of migrations (index: migration number - 1).
var goMigrations = map[int]func(*sql.Tx, *[]string) error{
	5: migrateNodeIDs,
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(abs)+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One connection: SQLite has one writer anyway, and this rules out
	// SQLITE_BUSY between our own connections. Callers never nest queries.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SchemaVersion is the number of applied migrations.
func (s *Store) SchemaVersion() int {
	v, _ := s.Meta("schema_version")
	n, _ := strconv.Atoi(v)
	return n
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		return err
	}
	cur := s.SchemaVersion()
	if cur > len(migrations) {
		return fmt.Errorf("database schema %d is newer than this longarm-hub (%d)", cur, len(migrations))
	}
	for i := cur; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if fn := goMigrations[i]; fn != nil {
			if err := fn(tx, &s.MigrationNotes); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO meta(k,v) VALUES('schema_version',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, strconv.Itoa(i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- meta

// Meta reads a meta value ("" if unset).
func (s *Store) Meta(k string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta writes a meta value.
func (s *Store) SetMeta(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// DeleteMeta removes a meta value.
func (s *Store) DeleteMeta(k string) error {
	_, err := s.db.Exec(`DELETE FROM meta WHERE k=?`, k)
	return err
}

// ---------------------------------------------------------------- nodes

// Node is a registered node (worker, client or admin).
type Node struct {
	// ID is the node's immutable ID (a UUID); Name its display name,
	// unique among non-revoked nodes, which can change (rename) and be
	// reused once the node is revoked.
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Role          string            `json:"role"`
	State         string            `json:"state"`                  // pending | approved | revoked
	Labels        map[string]string `json:"labels,omitempty"`       // admin-set (join code, SetLabels)
	HelloLabels   map[string]string `json:"hello_labels,omitempty"` // from the worker's longarmd.toml
	Profile       string            `json:"profile,omitempty"`
	Hostname      string            `json:"hostname,omitempty"`
	OS            string            `json:"os,omitempty"`
	Arch          string            `json:"arch,omitempty"`
	Version       string            `json:"version,omitempty"`
	Endpoints     []string          `json:"endpoints,omitempty"`
	CSR           string            `json:"csr,omitempty"`         // pending enrollment
	PollTokenHash string            `json:"poll_token,omitempty"`  // pending enrollment
	IssuedCert    string            `json:"issued_cert,omitempty"` // approved, not yet collected (JoinStatus)
	JoinedAt      time.Time         `json:"joined_at"`
	ApprovedAt    time.Time         `json:"approved_at,omitzero"`
	LastSeen      time.Time         `json:"last_seen,omitzero"`
	CertSerial    string            `json:"cert_serial,omitempty"`
	CertNotAfter  time.Time         `json:"cert_not_after,omitzero"`
	AdminDrain    bool              `json:"admin_drain,omitempty"`
	AuditOffset   int64             `json:"audit_offset,omitempty"`
	Mirrors       []string          `json:"mirrors,omitempty"`
	WarmRepos     []string          `json:"warm_repos,omitempty"`
	// Toolchain is the last ToolchainReport (protojson).
	Toolchain json.RawMessage `json:"toolchain,omitempty"`
	// Self-update (P2): release channel, whether the build can update, and
	// the last update outcome (protojson UpdateStatus).
	Channel       string          `json:"channel,omitempty"`
	UpdateEnabled bool            `json:"update_enabled,omitempty"`
	Update        json.RawMessage `json:"update,omitempty"`
	// Tailnet: the worker's last reported embedded tailscale node
	// (protojson TailnetState), the last key the hub minted for the node
	// (when, for which control server, why) and a pending admin re-key.
	Tailnet           json.RawMessage `json:"tailnet,omitempty"`
	TailnetKeyAt      time.Time       `json:"tailnet_key_at,omitzero"`
	TailnetKeyControl string          `json:"tailnet_key_control,omitempty"`
	TailnetKeyReason  string          `json:"tailnet_key_reason,omitempty"`
	TailnetRekey      bool            `json:"tailnet_rekey,omitempty"`
	// User is the Longarm user of a client/admin node's certificates
	// ("" = its name); Isolation the worker's [isolation] mode.
	User      string `json:"user,omitempty"`
	Isolation string `json:"isolation,omitempty"`
	// Owner is the user who registered a worker (a worker code;
	// "" = a shared fleet worker), Shares who else may use it, and
	// AccessVersion counts their changes (HubCommand.worker_access).
	Owner         string        `json:"owner,omitempty"`
	Shares        *WorkerShares `json:"shares,omitempty"`
	AccessVersion int64         `json:"access_version,omitempty"`
	// Admin: an admin worker (runs commands with administrator rights on
	// its machine), enrolled through a pairing's admin_worker_csr_pem. Set
	// at enrollment only; it shows as the label longarm.io/admin=true and
	// makes the worker owner-only (never shared) and never placed.
	Admin bool `json:"admin_worker,omitempty"`
	// Companion links a worker and its admin worker on the same machine:
	// the other one's node ID, recorded on both when a pairing enrolls them
	// together, an admin worker joins next to the owner's worker on its host,
	// or the migration pairs them. Shown as NodeInfo.companion_id /
	// companion / machine; package machine checks it still fits.
	Companion string `json:"companion,omitempty"`
	// CertName is the name in the node's current certificate (what
	// clients dial; "" = Name) and CertHasID whether that certificate
	// carries the node ID. After a rename they differ from Name until the
	// node renews.
	CertName  string `json:"cert_name,omitempty"`
	CertHasID bool   `json:"cert_has_id,omitempty"`
	// NodeKey is the end-to-end trust node key (base64) of the node's
	// last certificate: no other non-revoked node gets a certificate for it.
	NodeKey string `json:"node_key,omitempty"`
	// Capabilities the worker reported in its hello (NodeHello).
	Capabilities []string `json:"capabilities,omitempty"`
	// RetirePending: the node was renamed and got (or will get) a
	// certificate with its new name; once it presents one, every other
	// certificate of it is revoked. Renewals keep it set.
	RetirePending bool `json:"retire_pending,omitempty"`
}

// DialName is the name in the node's current certificate.
func (n *Node) DialName() string { return cmp.Or(n.CertName, n.Name) }

// WorkerShares: who besides its owner may use a worker.
type WorkerShares struct {
	Users    []string `json:"users,omitempty"`
	Teams    []string `json:"teams,omitempty"`
	Everyone bool     `json:"everyone,omitempty"`
}

// PutNode inserts or replaces a node (by ID). ErrNameTaken: another
// non-revoked node has its name (the database enforces rule 1 of
// internal/nodeid).
func (s *Store) PutNode(n *Node) error {
	if n.ID == "" {
		return errors.New("store: node without id")
	}
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO nodes(id,name,role,state,data) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, role=excluded.role, state=excluded.state, data=excluded.data`,
		n.ID, n.Name, n.Role, n.State, string(b))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("%w: %q", ErrNameTaken, n.Name)
	}
	if err == nil && s.OnNodePut != nil {
		s.OnNodePut(n)
	}
	return err
}

// DeleteNode removes a node record.
func (s *Store) DeleteNode(id string) error {
	_, err := s.db.Exec(`DELETE FROM nodes WHERE id=?`, id)
	return err
}

// Nodes returns all nodes.
func (s *Store) Nodes() ([]*Node, error) {
	rows, err := s.db.Query(`SELECT data FROM nodes ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		n := &Node{}
		if err := json.Unmarshal([]byte(d), n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- certs

// Cert is an issued certificate.
type Cert struct {
	Serial    string
	Node      string // the node's name when it was issued (as in the certificate)
	NodeID    string // the node it belongs to
	HasID     bool   // the certificate carries the node ID (issued since node IDs)
	NotAfter  time.Time
	IssuedAt  time.Time
	RevokedAt time.Time // zero = valid
}

// AddCert records an issued certificate.
func (s *Store) AddCert(c Cert) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO certs(serial,node,node_id,not_after,issued_at,has_id) VALUES(?,?,?,?,?,?)`,
		c.Serial, c.Node, c.NodeID, c.NotAfter.Unix(), c.IssuedAt.Unix(), c.HasID)
	return err
}

// CertNode is the node ID a recorded certificate belongs to ("" = the
// serial is not recorded).
func (s *Store) CertNode(serial string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT node_id FROM certs WHERE serial=?`, serial).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// NameHasCerts reports whether a certificate was ever recorded for the
// name (as issued; any node, revoked or expired ones too).
func (s *Store) NameHasCerts(name string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM certs WHERE node=?`, name).Scan(&n)
	return n > 0, err
}

// LegacyCertNames are the names of the certificates without a node ID
// recorded for the node (any state).
func (s *Store) LegacyCertNames(nodeID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT node FROM certs WHERE node_id=? AND has_id=0 ORDER BY node`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// RevokedName is a legacy name revocation (certificates without a node ID).
type RevokedName struct {
	Role, Name string
	NotAfter   time.Time
}

// AddRevokedName records (or extends) a legacy name revocation.
func (s *Store) AddRevokedName(r RevokedName) error {
	_, err := s.db.Exec(`INSERT INTO revoked_names(role,name,not_after) VALUES(?,?,?)
		ON CONFLICT(role,name) DO UPDATE SET not_after=max(not_after, excluded.not_after)`, r.Role, r.Name, r.NotAfter.Unix())
	return err
}

// ExtendRevokedNames makes every legacy name revocation last at least
// until t (the issuing CA's NotAfter: a certificate without an ID of a
// listed name can't outlive the CA).
func (s *Store) ExtendRevokedNames(t time.Time) error {
	_, err := s.db.Exec(`UPDATE revoked_names SET not_after=? WHERE not_after < ?`, t.Unix(), t.Unix())
	return err
}

// RevokedNames are the legacy name revocations in force at now.
func (s *Store) RevokedNames(now time.Time) ([]RevokedName, error) {
	rows, err := s.db.Query(`SELECT role,name,not_after FROM revoked_names WHERE not_after > ? ORDER BY role,name`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RevokedName
	for rows.Next() {
		var r RevokedName
		var na int64
		if err := rows.Scan(&r.Role, &r.Name, &na); err != nil {
			return nil, err
		}
		r.NotAfter = time.Unix(na, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// LegacyCerts maps node IDs to the serials of their valid (unrevoked,
// unexpired) certificates without a node ID.
func (s *Store) LegacyCerts(now time.Time) (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT node_id, serial FROM certs WHERE has_id=0 AND revoked_at IS NULL AND not_after > ? AND node_id != '' ORDER BY serial`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, serial string
		if err := rows.Scan(&id, &serial); err != nil {
			return nil, err
		}
		out[id] = append(out[id], serial)
	}
	return out, rows.Err()
}

// RevokeCertsExcept revokes every unexpired certificate of the node with
// ID nodeID but keep (superseded by keep: a renamed node's older ones).
func (s *Store) RevokeCertsExcept(nodeID, keep string, at time.Time) (int64, error) {
	r, err := s.db.Exec(`UPDATE certs SET revoked_at=? WHERE node_id=? AND serial != ? AND revoked_at IS NULL AND not_after > ?`,
		at.Unix(), nodeID, keep, at.Unix())
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// RevokeNodeCerts revokes every unexpired certificate of the node with
// ID nodeID. Revocation is by ID: certificates of another node that had
// the same name stay valid, and so do those of a node that gets the name
// after this one.
func (s *Store) RevokeNodeCerts(nodeID string, at time.Time) (int64, error) {
	if nodeID == "" {
		return 0, errors.New("store: revoke without node id")
	}
	r, err := s.db.Exec(`UPDATE certs SET revoked_at=? WHERE node_id=? AND revoked_at IS NULL`, at.Unix(), nodeID)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// CertNodes maps the serial of every recorded, unexpired certificate to
// its node ID (legacy certificates without an ID find their node by it).
func (s *Store) CertNodes(now time.Time) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT serial,node_id FROM certs WHERE not_after > ?`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var serial, id string
		if err := rows.Scan(&serial, &id); err != nil {
			return nil, err
		}
		out[serial] = id
	}
	return out, rows.Err()
}

// RevokedCerts returns revoked certificates that have not expired yet.
func (s *Store) RevokedCerts(now time.Time) ([]Cert, error) {
	rows, err := s.db.Query(`SELECT serial,node,node_id,not_after,issued_at,revoked_at FROM certs
		WHERE revoked_at IS NOT NULL AND not_after > ? ORDER BY serial`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cert
	for rows.Next() {
		var c Cert
		var na, ia, ra int64
		if err := rows.Scan(&c.Serial, &c.Node, &c.NodeID, &na, &ia, &ra); err != nil {
			return nil, err
		}
		c.NotAfter, c.IssuedAt, c.RevokedAt = time.Unix(na, 0), time.Unix(ia, 0), time.Unix(ra, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CertRevoked reports whether serial is revoked.
func (s *Store) CertRevoked(serial string) (bool, error) {
	var ra sql.NullInt64
	err := s.db.QueryRow(`SELECT revoked_at FROM certs WHERE serial=?`, serial).Scan(&ra)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return ra.Valid, err
}

// PruneCerts removes certificates that expired before t.
func (s *Store) PruneCerts(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM certs WHERE not_after < ?`, t.Unix())
	return err
}

// ---------------------------------------------------------------- join codes

// JoinCode is a one-time enrollment code (only its hash is stored).
type JoinCode struct {
	ID        string            `json:"id"`
	Role      string            `json:"role"`
	Name      string            `json:"name,omitempty"`
	User      string            `json:"user,omitempty"`  // client/admin: the Longarm user of the certificate
	Owner     string            `json:"owner,omitempty"` // worker: the user who will own it
	Labels    map[string]string `json:"labels,omitempty"`
	Profile   string            `json:"profile,omitempty"`
	CreatedBy string            `json:"created_by"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt time.Time         `json:"expires_at"`
	UsedAt    time.Time         `json:"used_at,omitzero"`
	UsedBy    string            `json:"used_by,omitempty"`
}

// AddJoinCode stores a code by its hash.
func (s *Store) AddJoinCode(hash string, c *JoinCode) error {
	b, _ := json.Marshal(c)
	_, err := s.db.Exec(`INSERT INTO join_codes(id,hash,data,expires_at) VALUES(?,?,?,?)`, c.ID, hash, string(b), c.ExpiresAt.Unix())
	return err
}

// UseJoinCode atomically marks an unused, unexpired code as used by node
// and returns it; ErrNotFound if there is no such usable code.
func (s *Store) UseJoinCode(hash, node string, now time.Time) (*JoinCode, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var d string
	err = tx.QueryRow(`SELECT data FROM join_codes WHERE hash=? AND used_at IS NULL AND expires_at > ?`, hash, now.Unix()).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c := &JoinCode{}
	if err := json.Unmarshal([]byte(d), c); err != nil {
		return nil, err
	}
	c.UsedAt, c.UsedBy = now, node
	b, _ := json.Marshal(c)
	if _, err := tx.Exec(`UPDATE join_codes SET used_at=?, data=? WHERE hash=?`, now.Unix(), string(b), hash); err != nil {
		return nil, err
	}
	return c, tx.Commit()
}

// DeleteOwnedJoinCodes deletes the unused worker codes owned by owner and
// returns their ids.
func (s *Store) DeleteOwnedJoinCodes(owner string) ([]string, error) {
	rows, err := s.db.Query(`SELECT id, data FROM join_codes WHERE used_at IS NULL`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id, d string
		if err := rows.Scan(&id, &d); err != nil {
			rows.Close()
			return nil, err
		}
		c := &JoinCode{}
		if json.Unmarshal([]byte(d), c) == nil && c.Owner == owner && c.UsedAt.IsZero() {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := s.db.Exec(`DELETE FROM join_codes WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// JoinCodes lists codes (newest first), dropping ones expired for a day.
func (s *Store) JoinCodes(now time.Time) ([]*JoinCode, error) {
	if _, err := s.db.Exec(`DELETE FROM join_codes WHERE expires_at < ?`, now.Add(-24*time.Hour).Unix()); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT data FROM join_codes ORDER BY expires_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JoinCode
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		c := &JoinCode{}
		if json.Unmarshal([]byte(d), c) == nil {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- jobs

// JobRow is a cached job (data = the marshalled pb.Job).
type JobRow struct {
	Worker    string // the worker's node ID
	ID        string
	State     int32
	UpdatedAt time.Time
	Data      []byte
}

// PutJob upserts a cached job.
func (s *Store) PutJob(j JobRow) error {
	_, err := s.db.Exec(`INSERT INTO jobs(worker,id,state,updated_at,data) VALUES(?,?,?,?,?)
		ON CONFLICT(worker,id) DO UPDATE SET state=excluded.state, updated_at=excluded.updated_at, data=excluded.data`,
		j.Worker, j.ID, j.State, j.UpdatedAt.UnixMilli(), j.Data)
	return err
}

// DeleteJob removes a cached job.
func (s *Store) DeleteJob(worker, id string) error {
	_, err := s.db.Exec(`DELETE FROM jobs WHERE worker=? AND id=?`, worker, id)
	return err
}

// ReplaceWorkerJobs replaces the cached jobs of worker (a full report).
func (s *Store) ReplaceWorkerJobs(worker string, rows []JobRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM jobs WHERE worker=?`, worker); err != nil {
		return err
	}
	for _, j := range rows {
		if _, err := tx.Exec(`INSERT INTO jobs(worker,id,state,updated_at,data) VALUES(?,?,?,?,?)`,
			j.Worker, j.ID, j.State, j.UpdatedAt.UnixMilli(), j.Data); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Jobs returns every cached job.
func (s *Store) Jobs() ([]JobRow, error) {
	rows, err := s.db.Query(`SELECT worker,id,state,updated_at,data FROM jobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var j JobRow
		var ua int64
		if err := rows.Scan(&j.Worker, &j.ID, &j.State, &ua, &j.Data); err != nil {
			return nil, err
		}
		j.UpdatedAt = time.UnixMilli(ua)
		out = append(out, j)
	}
	return out, rows.Err()
}

// ClearJobs empties the job cache (`longarm-hub serve --rebuild-jobs`).
func (s *Store) ClearJobs() error {
	_, err := s.db.Exec(`DELETE FROM jobs`)
	return err
}

// ---------------------------------------------------------------- audit

// AuditRow is one forwarded audit record.
type AuditRow struct {
	Node   string // the node's name at the time ("hub": the hub's own records)
	NodeID string // the node's ID ("" for the hub's own records)
	Time   time.Time
	Who    string
	Method string
	Line   string
	// attribution at write time (migration 5); Attributed false: older row
	ActorUser   string
	SubjectUser string
	Attributed  bool
}

// AddAudit stores records.
func (s *Store) AddAudit(rows []AuditRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT INTO audit(node,node_id,time,who,method,line,actor_user,subject_user,attributed) VALUES(?,?,?,?,?,?,?,?,?)`,
			r.Node, r.NodeID, r.Time.UnixMilli(), r.Who, r.Method, r.Line, r.ActorUser, r.SubjectUser, r.Attributed); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Audit returns records, newest first: of the node with ID nodeID, or
// with node name node (e.g. "hub"), or all ("", "").
func (s *Store) Audit(nodeID, node string, since time.Time, limit int) ([]AuditRow, error) {
	if limit <= 0 || limit > 10000 {
		limit = 200
	}
	q := `SELECT node,node_id,time,who,method,line,actor_user,subject_user,attributed FROM audit WHERE time >= ?`
	args := []any{since.UnixMilli()}
	switch {
	case nodeID != "":
		q += ` AND node_id = ?`
		args = append(args, nodeID)
	case node != "":
		q += ` AND node = ?`
		args = append(args, node)
	}
	q += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var t int64
		if err := rows.Scan(&r.Node, &r.NodeID, &t, &r.Who, &r.Method, &r.Line, &r.ActorUser, &r.SubjectUser, &r.Attributed); err != nil {
			return nil, err
		}
		r.Time = time.UnixMilli(t)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- users

// User is a local web panel account.
type User struct {
	Name       string    `json:"name"`
	PassHash   string    `json:"pass_hash"` // argon2id, PHC-like encoding
	TOTPSecret string    `json:"totp_secret"`
	CreatedAt  time.Time `json:"created_at"`
	LastTOTP   int64     `json:"last_totp,omitempty"` // last accepted time step (replay protection)
	// Role: "" or "admin" = a panel administrator (everything); "user" = a
	// Longarm user (Tenant) seeing only their own jobs (multi-tenancy).
	Role   string `json:"role,omitempty"`
	Tenant string `json:"tenant,omitempty"`
	// Entra is the object ID of the Microsoft Entra user signing in to this
	// account (web sign-in; such an account may have no password).
	Entra string `json:"entra,omitempty"`
}

// Admin reports whether the panel account administers the hub.
func (u *User) Admin() bool { return u.Role == "" || u.Role == "admin" }

// PutUser inserts or replaces a user.
func (s *Store) PutUser(u *User) error {
	b, _ := json.Marshal(u)
	_, err := s.db.Exec(`INSERT INTO users(name,data) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET data=excluded.data`, u.Name, string(b))
	return err
}

// GetUser loads a user.
func (s *Store) GetUser(name string) (*User, error) {
	var d string
	err := s.db.QueryRow(`SELECT data FROM users WHERE name=?`, name).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u := &User{}
	return u, json.Unmarshal([]byte(d), u)
}

// Users lists user names.
func (s *Store) Users() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DeleteUser removes a user and their sessions.
func (s *Store) DeleteUser(name string) error {
	if _, err := s.db.Exec(`DELETE FROM web_sessions WHERE user=?`, name); err != nil {
		return err
	}
	r, err := s.db.Exec(`DELETE FROM users WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddWebSession stores a login session by token hash.
func (s *Store) AddWebSession(hash, user string, expires time.Time) error {
	_, err := s.db.Exec(`INSERT INTO web_sessions(hash,user,expires_at) VALUES(?,?,?)`, hash, user, expires.Unix())
	return err
}

// WebSession returns the user of a valid session.
func (s *Store) WebSession(hash string, now time.Time) (string, error) {
	var u string
	err := s.db.QueryRow(`SELECT user FROM web_sessions WHERE hash=? AND expires_at > ?`, hash, now.Unix()).Scan(&u)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return u, err
}

// DeleteWebSession logs a session out; expired sessions are pruned too.
func (s *Store) DeleteWebSession(hash string, now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE hash=? OR expires_at < ?`, hash, now.Unix())
	return err
}

// ---------------------------------------------------------------- profiles

// PutProfile stores a toolchain profile (protojson of pb.Profile).
func (s *Store) PutProfile(name string, data []byte) error {
	_, err := s.db.Exec(`INSERT INTO profiles(name,data) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET data=excluded.data`, name, string(data))
	return err
}

// Profiles returns name -> data.
func (s *Store) Profiles() (map[string][]byte, error) {
	rows, err := s.db.Query(`SELECT name,data FROM profiles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var n, d string
		if err := rows.Scan(&n, &d); err != nil {
			return nil, err
		}
		out[n] = []byte(d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- releases

// Release is one file of a released version.
type Release struct {
	Version string
	File    string // e.g. "longarmd-linux-amd64"
	OS      string
	Arch    string
	SHA256  string
	Size    int64
	AddedAt time.Time
	Channel string // stable (default), beta ...
	Kind    string // binary | package (msi, deb, rpm, pkg, tar.gz, zip)
	Sig     []byte // the .minisig file, if signed
}

// AddRelease records a release file (replacing a same-named one).
func (s *Store) AddRelease(r Release) error {
	_, err := s.db.Exec(`INSERT INTO releases(version,file,os,arch,sha256,size,added_at,channel,kind,sig) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(version,file) DO UPDATE SET os=excluded.os, arch=excluded.arch, sha256=excluded.sha256, size=excluded.size,
		added_at=excluded.added_at, channel=excluded.channel, kind=excluded.kind, sig=excluded.sig`,
		r.Version, r.File, r.OS, r.Arch, r.SHA256, r.Size, r.AddedAt.Unix(), cmp.Or(r.Channel, "stable"), cmp.Or(r.Kind, "binary"), r.Sig)
	return err
}

// Releases lists release files, newest version first.
func (s *Store) Releases() ([]Release, error) {
	rows, err := s.db.Query(`SELECT version,file,os,arch,sha256,size,added_at,channel,kind,sig FROM releases ORDER BY added_at DESC, file`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		var a int64
		if err := rows.Scan(&r.Version, &r.File, &r.OS, &r.Arch, &r.SHA256, &r.Size, &a, &r.Channel, &r.Kind, &r.Sig); err != nil {
			return nil, err
		}
		r.AddedAt = time.Unix(a, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PeekJoinCode returns a usable code without using it.
func (s *Store) PeekJoinCode(hash string, now time.Time) (*JoinCode, error) {
	var d string
	err := s.db.QueryRow(`SELECT data FROM join_codes WHERE hash=? AND used_at IS NULL AND expires_at > ?`, hash, now.Unix()).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c := &JoinCode{}
	return c, json.Unmarshal([]byte(d), c)
}

// DeleteUserSessions ends every panel session of a user (password reset).
func (s *Store) DeleteUserSessions(name string) error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE user=?`, name)
	return err
}

// ---------------------------------------------------------------- tenants

// Tenant is a Longarm user (multi-tenancy): teams, quota overrides,
// disabled flag. Data is the protojson of pb.TenantUser.
type Tenant struct {
	Name string
	Data []byte
}

// PutTenant inserts or replaces a tenant.
func (s *Store) PutTenant(name string, data []byte) error {
	_, err := s.db.Exec(`INSERT INTO tenants(name,data) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET data=excluded.data`, name, string(data))
	return err
}

// DeleteTenant removes a tenant.
func (s *Store) DeleteTenant(name string) error {
	_, err := s.db.Exec(`DELETE FROM tenants WHERE name=?`, name)
	return err
}

// Tenants returns every tenant.
func (s *Store) Tenants() ([]Tenant, error) {
	rows, err := s.db.Query(`SELECT name,data FROM tenants ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		var d string
		if err := rows.Scan(&t.Name, &d); err != nil {
			return nil, err
		}
		t.Data = []byte(d)
		out = append(out, t)
	}
	return out, rows.Err()
}

// PortLinkRow is a stored port link.
type PortLinkRow struct {
	ID, Owner string
	Data      []byte // protojson PortLink
}

// PutPortLink inserts or replaces a port link.
func (s *Store) PutPortLink(r PortLinkRow) error {
	_, err := s.db.Exec(`INSERT INTO port_links(id,owner,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET owner=excluded.owner, data=excluded.data`,
		r.ID, r.Owner, string(r.Data))
	return err
}

// DeletePortLink removes a port link.
func (s *Store) DeletePortLink(id string) error {
	_, err := s.db.Exec(`DELETE FROM port_links WHERE id=?`, id)
	return err
}

// PortLinks returns every port link.
func (s *Store) PortLinks() ([]PortLinkRow, error) {
	rows, err := s.db.Query(`SELECT id,owner,data FROM port_links ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortLinkRow
	for rows.Next() {
		var r PortLinkRow
		var d string
		if err := rows.Scan(&r.ID, &r.Owner, &d); err != nil {
			return nil, err
		}
		r.Data = []byte(d)
		out = append(out, r)
	}
	return out, rows.Err()
}
