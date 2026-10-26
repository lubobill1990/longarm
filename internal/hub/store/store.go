// Package store is longarm-hub's persistence: one SQLite database (pure Go
// driver modernc.org/sqlite, no cgo) in WAL mode, so Litestream can stream
// it to object storage (DESIGN §3.3). Schema changes are numbered migrations
// recorded in the meta table.
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
	"time"

	_ "modernc.org/sqlite" // registers "sqlite"
)

// Store is the hub database.
type Store struct {
	db *sql.DB
}

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
}

// WorkerShares: who besides its owner may use a worker.
type WorkerShares struct {
	Users    []string `json:"users,omitempty"`
	Teams    []string `json:"teams,omitempty"`
	Everyone bool     `json:"everyone,omitempty"`
}

// PutNode inserts or replaces a node.
func (s *Store) PutNode(n *Node) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO nodes(name,role,state,data) VALUES(?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET role=excluded.role, state=excluded.state, data=excluded.data`,
		n.Name, n.Role, n.State, string(b))
	return err
}

// DeleteNode removes a node record.
func (s *Store) DeleteNode(name string) error {
	_, err := s.db.Exec(`DELETE FROM nodes WHERE name=?`, name)
	return err
}

// Nodes returns all nodes.
func (s *Store) Nodes() ([]*Node, error) {
	rows, err := s.db.Query(`SELECT data FROM nodes ORDER BY name`)
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
	Node      string
	NotAfter  time.Time
	IssuedAt  time.Time
	RevokedAt time.Time // zero = valid
}

// AddCert records an issued certificate.
func (s *Store) AddCert(c Cert) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO certs(serial,node,not_after,issued_at) VALUES(?,?,?,?)`,
		c.Serial, c.Node, c.NotAfter.Unix(), c.IssuedAt.Unix())
	return err
}

// RevokeNodeCerts revokes every unexpired certificate of node.
func (s *Store) RevokeNodeCerts(node string, at time.Time) (int64, error) {
	r, err := s.db.Exec(`UPDATE certs SET revoked_at=? WHERE node=? AND revoked_at IS NULL`, at.Unix(), node)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// RevokedCerts returns revoked certificates that have not expired yet.
func (s *Store) RevokedCerts(now time.Time) ([]Cert, error) {
	rows, err := s.db.Query(`SELECT serial,node,not_after,issued_at,revoked_at FROM certs
		WHERE revoked_at IS NOT NULL AND not_after > ? ORDER BY serial`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cert
	for rows.Next() {
		var c Cert
		var na, ia, ra int64
		if err := rows.Scan(&c.Serial, &c.Node, &na, &ia, &ra); err != nil {
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
	Worker    string
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
	Node   string
	Time   time.Time
	Who    string
	Method string
	Line   string
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
		if _, err := tx.Exec(`INSERT INTO audit(node,time,who,method,line) VALUES(?,?,?,?,?)`,
			r.Node, r.Time.UnixMilli(), r.Who, r.Method, r.Line); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Audit returns records, newest first.
func (s *Store) Audit(node string, since time.Time, limit int) ([]AuditRow, error) {
	if limit <= 0 || limit > 10000 {
		limit = 200
	}
	q := `SELECT node,time,who,method,line FROM audit WHERE time >= ?`
	args := []any{since.UnixMilli()}
	if node != "" {
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
		if err := rows.Scan(&r.Node, &t, &r.Who, &r.Method, &r.Line); err != nil {
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
