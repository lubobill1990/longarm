package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/nodeid"
)

// schema4 makes a database as a hub before node IDs (schema version 4)
// left it, with rows like the live hub's.
func schema4(t *testing.T, path string) {
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
	for _, m := range migrations[:4] {
		exec(m)
	}
	exec(`INSERT INTO meta(k,v) VALUES('schema_version','4')`)
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
	for _, c := range []struct{ serial, node string }{{"a1", "alice-pc"}, {"a2", "old-box"}, {"a3", "cafe"}, {"a4", "1234"}} {
		exec(`INSERT INTO certs(serial,node,not_after,issued_at) VALUES(?,?,?,?)`, c.serial, c.node, now.Add(time.Hour).Unix(), now.Unix())
	}
	exec(`UPDATE certs SET revoked_at=? WHERE node='old-box'`, now.Unix())
	exec(`INSERT INTO audit(node,time,who,method,line) VALUES('alice-pc',?,'client/root','/x','{}')`, now.UnixMilli())
	exec(`INSERT INTO audit(node,time,who,method,line) VALUES('hub',?,'web/root','users/add','{}')`, now.UnixMilli())
	exec(`INSERT INTO jobs(worker,id,state,updated_at,data) VALUES('alice-pc','J1',2,?,x'00')`, now.UnixMilli())
	exec(`INSERT INTO port_links(id,owner,data) VALUES('L1','alice','{"id":"L1","owner":"alice","from":"alice-pc","to":"cafe","port":80}')`)
}

// TestMigrateNodeIDs: schema 4 -> 5 keeps every node and its data, gives
// each an ID, renames names that look like IDs (audited, noted), maps
// certificates, audit records, cached jobs and port links to IDs, links a
// worker and its admin worker, and enforces unique active names.
func TestMigrateNodeIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	schema4(t, path)
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
	if len(nodes) != 15 {
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
	// companion link: the admin worker and the worker on its host
	aw, w := by["cpc-bob-x7k2h-admin"], by["bob-cpc-bob-x7k2h"]
	if aw.Companion != w.ID || w.Companion != aw.ID {
		t.Fatalf("companions %q %q", aw.Companion, w.Companion)
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
	if len(again) != 16 {
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
