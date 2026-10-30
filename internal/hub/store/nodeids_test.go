package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/nodeid"
)

// liveWorkers is how many plain workers schema5 adds (the live hub's
// size), next to the special cases.
const liveWorkers = 30

// schema5 makes a database as a hub before node IDs left it - schema
// version 5 (#60's audit attribution: the last schema released before node
// IDs) - with rows like the live hub's: user-owned and fleet workers,
// admin workers, devices of users that sign in (with and without a pinned
// user), revoked nodes, certificates, attributed and older audit records,
// cached jobs, port links.
func schema5(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`)
	for _, m := range migrations[:5] {
		exec(m)
	}
	exec(`INSERT INTO meta(k,v) VALUES('schema_version','5')`)
	exec(`INSERT INTO meta(k,v) VALUES('revocation_version','7')`) // workers synced version 7
	now := time.Now()
	node := func(name, role, state string, extra map[string]any) {
		d := map[string]any{"name": name, "role": role, "state": state, "joined_at": now.UTC()}
		for k, v := range extra {
			d[k] = v
		}
		b, _ := json.Marshal(d)
		exec(`INSERT INTO nodes(name,role,state,data) VALUES(?,?,?,?)`, name, role, state, string(b))
	}
	// workers shaped like a live hub's ...
	node("alice-pair", "worker", "approved", map[string]any{"owner": "alice", "hostname": "pair"})
	node("alice-pc", "worker", "approved", map[string]any{"hostname": "alice-pc"})
	node("e2e-worker", "worker", "approved", nil)
	node("spike-1", "worker", "approved", nil)
	node("bob-cpc-bob-x7k2h", "worker", "approved", map[string]any{"owner": "bob", "hostname": "CPC-bob-X7K2H"})
	node("cpc-bob-x7k2h-admin", "worker", "approved", map[string]any{"owner": "bob", "hostname": "cpc-bob-x7k2h", "admin_worker": true})
	// ... admins and clients ...
	// a pair a hub with machine grouping (by name) recorded; a stale link
	node("carol-box", "worker", "approved", map[string]any{"owner": "carol", "companion": "carol-sys"})
	node("carol-sys", "worker", "approved", map[string]any{"owner": "carol", "admin_worker": true, "companion": "carol-box"})
	node("dan-box", "worker", "approved", map[string]any{"owner": "dan", "companion": "ghost"})
	node("root", "admin", "approved", nil)
	node("alice-laptop", "client", "approved", map[string]any{"user": "alice"})
	// ... a revoked one, and names that break the new rules
	node("old-box", "worker", "revoked", nil)
	node("cafe", "worker", "approved", nil)
	node("1234", "client", "approved", nil) // its user, "1234", can't be carried under another name
	node("beef", "client", "approved", nil) // user "beef" (its name): pinned before the rename
	node("w-dead", "worker", "approved", nil)
	node("dead", "worker", "approved", nil)  // wants w-dead, which an active node has
	node("w-cafe", "worker", "revoked", nil) // the name "cafe" will want
	for i := range liveWorkers {             // the bulk: plain workers, owned and fleet
		extra := map[string]any{"hostname": fmt.Sprintf("host-%02d", i), "version": "0.1.13", "labels": map[string]string{"pool": "p"}}
		if i%3 != 0 {
			extra["owner"] = fmt.Sprintf("user%d", i%5)
		}
		node(fmt.Sprintf("live-%02d", i), "worker", "approved", extra)
	}
	for _, c := range []struct{ serial, node string }{{"a1", "alice-pc"}, {"a2", "old-box"}, {"a3", "cafe"}, {"a4", "1234"}} {
		exec(`INSERT INTO certs(serial,node,not_after,issued_at) VALUES(?,?,?,?)`, c.serial, c.node, now.Add(time.Hour).Unix(), now.Unix())
	}
	exec(`UPDATE certs SET revoked_at=? WHERE node='old-box'`, now.Unix())
	exec(`INSERT INTO audit(node,time,who,method,line) VALUES('alice-pc',?,'client/root','/x','{}')`, now.UnixMilli())
	exec(`INSERT INTO audit(node,time,who,method,line) VALUES('hub',?,'web/root','users/add','{}')`, now.UnixMilli())
	// written by a schema-5 hub: attributed (#60)
	exec(`INSERT INTO audit(node,time,who,method,line,actor_user,subject_user,attributed) VALUES('bob-cpc-bob-x7k2h',?,'client/bob-laptop','/longarm.v1.Jobs/Submit','{}','bob','bob',1)`, now.UnixMilli())
	exec(`INSERT INTO jobs(worker,id,state,updated_at,data) VALUES('alice-pc','J1',2,?,x'00')`, now.UnixMilli())
	exec(`INSERT INTO port_links(id,owner,data) VALUES('L1','alice','{"id":"L1","owner":"alice","from":"alice-pc","to":"cafe","port":80}')`)
}

// TestMigrateNodeIDs: schema 5 -> 6 keeps every node and its data, gives
// each an ID, renames names that look like IDs (audited, noted), maps
// certificates, audit records, cached jobs and port links to IDs, links a
// worker and its admin worker, and enforces unique active names.
func TestMigrateNodeIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	schema5(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.SchemaVersion() != len(migrations) {
		t.Fatalf("schema %d", s.SchemaVersion())
	}
	nodes, err := s.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*Node{} // active nodes by name (unique), else revoked ones
	ids := map[string]bool{}
	var revokedWCafe *Node
	for _, n := range nodes {
		if !nodeid.Valid(n.ID) || ids[n.ID] {
			t.Fatalf("node %s: id %q", n.Name, n.ID)
		}
		ids[n.ID] = true
		if n.Name == "w-cafe" && n.State == "revoked" {
			revokedWCafe = n
			continue
		}
		by[n.Name] = n
	}
	if len(nodes) != 18+liveWorkers {
		t.Fatalf("%d nodes", len(nodes))
	}
	for _, name := range []string{"alice-pair", "alice-pc", "e2e-worker", "spike-1", "cpc-bob-x7k2h-admin", "bob-cpc-bob-x7k2h", "root", "alice-laptop", "old-box"} {
		if by[name] == nil || by[name].CertName != "" {
			t.Fatalf("%s: %+v (the live hub's names stay)", name, by[name])
		}
	}
	if by["alice-pair"].Owner != "alice" || by["alice-laptop"].User != "alice" || by["old-box"].State != "revoked" {
		t.Fatal("node data lost")
	}
	// rule 2: renamed, the certificate keeps the old name until renewed;
	// the revoked w-cafe doesn't hold its name (rule 1: active nodes only)
	cafe, d1234 := by["w-cafe"], by["d-1234"]
	if cafe == nil || cafe.CertName != "cafe" || cafe.Role != "worker" || d1234 == nil || d1234.CertName != "1234" {
		t.Fatalf("renamed: %+v %+v (all: %v)", cafe, d1234, names(nodes))
	}
	// a device's implicit user (its name) is pinned before any rename: a
	// renewal under the new name keeps acting as that user, never as a
	// user named like the new name
	if b := by["d-beef"]; b == nil || b.User != "beef" || b.State != "approved" {
		t.Fatalf("d-beef: %+v", b)
	}
	if by["alice-laptop"].User != "alice" || by["root"].User != "root" {
		t.Fatal("devices' users pinned")
	}
	// one whose user can't be carried under another name: revoked, with
	// its certificates (enroll it again)
	if d1234.State != "revoked" || d1234.User != "1234" {
		t.Fatalf("d-1234: %+v", d1234)
	}
	// duplicates: dead wants w-dead, which an active node has: suffixed
	if by["w-dead"] == nil || by["w-dead-2"] == nil || by["w-dead-2"].CertName != "dead" {
		t.Fatalf("w-dead-2: %v", names(nodes))
	}
	if revokedWCafe == nil || cafe.State != "approved" {
		t.Fatal("the revoked w-cafe")
	}
	if len(s.MigrationNotes) != 5 || !strings.Contains(strings.Join(s.MigrationNotes, "\n"), `"cafe"`) ||
		!strings.Contains(strings.Join(s.MigrationNotes, "\n"), "revoked: its user") {
		t.Fatalf("notes %q", s.MigrationNotes)
	}
	rows, _ := s.Audit("", "hub", time.Time{}, 100)
	renames := 0
	for _, r := range rows {
		if r.Method == "nodes/rename" && r.Who == "hub/migration" {
			renames++
		}
	}
	if renames != 4 {
		t.Fatalf("%d rename audit records", renames)
	}
	// every live worker: its name, owner, labels and version, no rename
	for i := range liveWorkers {
		n := by[fmt.Sprintf("live-%02d", i)]
		if n == nil || n.CertName != "" || n.Version != "0.1.13" || n.Labels["pool"] != "p" || (i%3 != 0) != (n.Owner != "") {
			t.Fatalf("live-%02d: %+v", i, n)
		}
	}
	// #60's attribution survives; the migration's own records are
	// attributed to the renamed machine's user
	if a, _ := s.Audit(by["bob-cpc-bob-x7k2h"].ID, "", time.Time{}, 10); len(a) != 1 || !a[0].Attributed || a[0].ActorUser != "bob" || a[0].SubjectUser != "bob" {
		t.Fatalf("attributed record %+v", a)
	}
	for _, r := range rows {
		if r.Who == "hub/migration" && (!r.Attributed || r.ActorUser != "") {
			t.Fatalf("migration record %+v", r)
		}
		if r.Who == "hub/migration" && strings.Contains(r.Line, `"old_name":"beef"`) && r.SubjectUser != "beef" {
			t.Fatalf("d-beef's rename is not its user's: %+v", r)
		}
	}
	// companion link: the admin worker and the worker on its host
	aw, w := by["cpc-bob-x7k2h-admin"], by["bob-cpc-bob-x7k2h"]
	if aw.Companion != w.ID || w.Companion != aw.ID {
		t.Fatalf("companions %q %q", aw.Companion, w.Companion)
	}
	if cb, cs := by["carol-box"], by["carol-sys"]; cb.Companion != cs.ID || cs.Companion != cb.ID {
		t.Fatalf("recorded pair (by name) not carried over by ID: %q %q", cb.Companion, cs.Companion)
	}
	if by["dan-box"].Companion != "" {
		t.Fatalf("stale companion kept: %q", by["dan-box"].Companion)
	}
	if by["alice-pc"].Companion != "" {
		t.Fatal("a worker without an admin worker got a companion")
	}
	// certificates: by node id; the revoked node's stay revoked
	cn, _ := s.CertNodes(time.Now())
	if cn["a1"] != by["alice-pc"].ID || cn["a2"] != by["old-box"].ID || cn["a3"] != cafe.ID {
		t.Fatalf("cert nodes %v", cn)
	}
	rc, _ := s.RevokedCerts(time.Now())
	if len(rc) != 2 || rc[0].NodeID != by["old-box"].ID || rc[1].NodeID != d1234.ID {
		t.Fatalf("revoked %+v", rc)
	}
	// legacy name tombstones for every node revoked before the upgrade and
	// by it (their original names, for certificates without an ID), and a
	// new revocation list version, so synced workers fetch the list again
	tomb, _ := s.RevokedNames(time.Now())
	got := map[string]bool{}
	for _, r := range tomb {
		got[r.Role+"/"+r.Name] = true
		if r.NotAfter.Before(time.Now().Add(LegacyNameTombstoneTTL - time.Hour)) {
			t.Fatalf("tombstone %+v too short", r)
		}
	}
	if len(tomb) != 3 || !got["worker/old-box"] || !got["worker/w-cafe"] || !got["client/1234"] {
		t.Fatalf("tombstones %v", got)
	}
	if v, _ := s.Meta("revocation_version"); v != "8" {
		t.Fatalf("revocation version %q, want 8", v)
	}
	// extended to the CA's lifetime when that is longer
	ca := time.Now().Add(10 * 365 * 24 * time.Hour).Truncate(time.Second)
	if err := s.ExtendRevokedNames(ca); err != nil {
		t.Fatal(err)
	}
	tomb, _ = s.RevokedNames(time.Now())
	for _, r := range tomb {
		if !r.NotAfter.Equal(ca) {
			t.Fatalf("tombstone %+v not extended to %s", r, ca)
		}
	}
	// audit, jobs, port links
	if a, _ := s.Audit(by["alice-pc"].ID, "", time.Time{}, 10); len(a) != 1 || a[0].Node != "alice-pc" {
		t.Fatalf("audit by id %+v", a)
	}
	jobs, _ := s.Jobs()
	if len(jobs) != 1 || jobs[0].Worker != by["alice-pc"].ID {
		t.Fatalf("jobs %+v", jobs)
	}
	links, _ := s.PortLinks()
	var l map[string]any
	json.Unmarshal(links[0].Data, &l)
	if l["fromId"] != by["alice-pc"].ID || l["toId"] != cafe.ID || l["to"] != "cafe" {
		t.Fatalf("link %v", l)
	}
	// rule 1 in the database: no second active node of a name ...
	if err := s.PutNode(&Node{ID: nodeid.New(), Name: "alice-pc", Role: "worker", State: "approved"}); err == nil || !strings.Contains(err.Error(), ErrNameTaken.Error()) {
		t.Fatalf("duplicate active name: %v", err)
	}
	// ... but a revoked node's name is free
	if err := s.PutNode(&Node{ID: nodeid.New(), Name: "old-box", Role: "worker", State: "approved"}); err != nil {
		t.Fatalf("reusing a revoked node's name: %v", err)
	}
	// reopening doesn't migrate again
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if len(s2.MigrationNotes) != 0 {
		t.Fatalf("migrated again: %v", s2.MigrationNotes)
	}
	again, _ := s2.Nodes()
	if len(again) != 19+liveWorkers {
		t.Fatalf("%d nodes after reopening", len(again))
	}
}

func names(nodes []*Node) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.Name+"("+n.State+")")
	}
	return out
}

// TestMigrateNodeIDsAtomic: migration 6 runs in one transaction with its
// schema version: a failure anywhere (here: after its Go step did all its
// work) leaves the schema-5 database exactly as it was - a hub that can't
// migrate refuses to start and the old hub can still open it - and the
// next start migrates from scratch, once.
func TestMigrateNodeIDsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	schema5(t, path)
	real := goMigrations[5]
	goMigrations[5] = func(tx *sql.Tx, notes *[]string) error {
		if err := real(tx, notes); err != nil {
			return err
		}
		return errors.New("injected failure")
	}
	_, err := Open(path)
	goMigrations[5] = real
	if err == nil || !strings.Contains(err.Error(), "migration 6: injected failure") {
		t.Fatalf("open with a failing migration: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	var v string
	var nodes, auditRows, idCols int
	db.QueryRow(`SELECT v FROM meta WHERE k='schema_version'`).Scan(&v)
	db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&nodes)
	db.QueryRow(`SELECT count(*) FROM audit`).Scan(&auditRows)
	db.QueryRow(`SELECT count(*) FROM pragma_table_info('nodes') WHERE name='id'`).Scan(&idCols)
	var tables int
	db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('nodes_v6','revoked_names')`).Scan(&tables)
	db.Close()
	if v != "5" || nodes != 18+liveWorkers || auditRows != 3 || idCols != 0 || tables != 0 {
		t.Fatalf("after a failed migration: schema %s, %d nodes, %d audit rows, id column %d, new tables %d", v, nodes, auditRows, idCols, tables)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	all, _ := s.Nodes()
	if s.SchemaVersion() != 6 || len(all) != 18+liveWorkers || len(s.MigrationNotes) != 5 {
		t.Fatalf("migrated: schema %d, %d nodes, notes %v", s.SchemaVersion(), len(all), s.MigrationNotes)
	}
	rows, _ := s.Audit("", "hub", time.Time{}, 100)
	renames := 0
	for _, r := range rows {
		if r.Who == "hub/migration" {
			renames++
		}
	}
	if renames != 4 {
		t.Fatalf("%d migration records (the failed attempt left some?)", renames)
	}
}

// TestNewerSchemaRefused: a hub refuses a database a newer hub migrated -
// the rollback story is restoring the pre-deploy snapshot, never running
// the older hub on the migrated database.
func TestNewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetMeta("schema_version", strconv.Itoa(len(migrations)+1))
	s.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this longarm-hub") {
		t.Fatalf("newer schema: %v", err)
	}
}
