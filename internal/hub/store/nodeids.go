package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/nodeid"
)

// migrateNodeIDs is migration 5's Go step (docs/security.md "Node
// identity", "Migration"): every node gets an ID; names that break the
// naming rules (one that looks like an ID; a name two non-revoked nodes
// share) are changed, with an audit record and a note for the log;
// certificates, audit records, cached jobs and port links get the IDs of
// their nodes; a worker and the admin worker paired on the same machine
// get a companion link.
func migrateNodeIDs(tx *sql.Tx, notes *[]string) error {
	type row struct {
		name, role, state string
		data              map[string]any
		id, newName       string
	}
	rs, err := tx.Query(`SELECT name, role, state, data FROM nodes`)
	if err != nil {
		return err
	}
	var all []*row
	for rs.Next() {
		r := &row{}
		var d string
		if err := rs.Scan(&r.name, &r.role, &r.state, &d); err != nil {
			rs.Close()
			return err
		}
		if err := json.Unmarshal([]byte(d), &r.data); err != nil {
			rs.Close()
			return fmt.Errorf("node %s: %w", r.name, err)
		}
		all = append(all, r)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	// Active nodes first (they keep their names when they can), then by name.
	sort.SliceStable(all, func(i, j int) bool {
		ai, aj := all[i].state != "revoked", all[j].state != "revoked"
		if ai != aj {
			return ai
		}
		return all[i].name < all[j].name
	})
	taken := map[string]bool{} // names of non-revoked nodes
	byName := map[string]*row{}
	now := time.Now().UTC()
	for _, r := range all {
		r.id = nodeid.New()
		r.newName = r.name
		reason := ""
		if nodeid.CheckName(r.name, nil) != nil {
			prefix := "d-"
			if r.role == "worker" {
				prefix = "w-"
			}
			r.newName, reason = prefix+r.name, "the name looks like a node id"
		}
		if r.state != "revoked" {
			base := r.newName
			for i := 2; taken[r.newName]; i++ {
				suffix := "-" + strconv.Itoa(i)
				r.newName = base[:min(len(base), 63-len(suffix))] + suffix
				if reason == "" {
					reason = "another active node has the name"
				}
			}
			taken[r.newName] = true
		}
		if _, dup := byName[r.name]; !dup {
			byName[r.name] = r
		}
		r.data["id"] = r.id
		if r.newName != r.name {
			r.data["name"] = r.newName
			// the certificate keeps the old name until renewed
			r.data["cert_name"] = r.name
			line, _ := json.Marshal(map[string]any{"time": now.Format(time.RFC3339Nano), "who": "hub/migration",
				"method": "nodes/rename", "id": r.id, "old_name": r.name, "new_name": r.newName, "reason": reason})
			if _, err := tx.Exec(`INSERT INTO audit(node,node_id,time,who,method,line) VALUES('hub','',?,?,?,?)`,
				now.UnixMilli(), "hub/migration", "nodes/rename", string(line)); err != nil {
				return err
			}
			*notes = append(*notes, fmt.Sprintf("node %q (%s, %s) renamed to %q: %s", r.name, r.role, r.state, r.newName, reason))
		}
	}
	// Companion links: an admin worker and the one plain worker of the
	// same owner on the same host (both non-revoked).
	str := func(r *row, k string) string { s, _ := r.data[k].(string); return s }
	for _, a := range all {
		if a.role != "worker" || a.state == "revoked" || a.data["admin_worker"] != true || str(a, "hostname") == "" {
			continue
		}
		var match []*row
		for _, w := range all {
			if w != a && w.role == "worker" && w.state != "revoked" && w.data["admin_worker"] != true &&
				str(w, "owner") == str(a, "owner") && strings.EqualFold(str(w, "hostname"), str(a, "hostname")) {
				match = append(match, w)
			}
		}
		if len(match) > 1 { // several workers on that host: the one the admin worker is named after
			var named []*row
			for _, w := range match {
				if w.name+"-admin" == a.name {
					named = append(named, w)
				}
			}
			match = named
		}
		if len(match) == 1 && str(match[0], "companion") == "" {
			a.data["companion"], match[0].data["companion"] = match[0].id, a.id
		}
	}
	for _, r := range all {
		b, err := json.Marshal(r.data)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO nodes_v5(id,name,role,state,data) VALUES(?,?,?,?,?)`,
			r.id, r.newName, r.role, r.state, string(b)); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`DROP TABLE nodes`,
		`ALTER TABLE nodes_v5 RENAME TO nodes`,
		`CREATE UNIQUE INDEX nodes_active_name ON nodes(name) WHERE state != 'revoked'`,
		`CREATE INDEX nodes_name ON nodes(name)`,
		`CREATE INDEX certs_node_id ON certs(node_id)`,
		`CREATE INDEX audit_node_id_time ON audit(node_id, time)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", strings.Fields(q)[0], err)
		}
	}
	// The names were the old primary key (one row per name): certificates
	// and audit records name their node by its old name.
	for old, r := range byName {
		for _, q := range []string{`UPDATE certs SET node_id=? WHERE node=?`, `UPDATE audit SET node_id=? WHERE node=? AND node != 'hub'`} {
			if _, err := tx.Exec(q, r.id, old); err != nil {
				return err
			}
		}
	}
	// Cached jobs: keyed by the worker's ID now.
	for old, r := range byName {
		if _, err := tx.Exec(`UPDATE jobs SET worker=? WHERE worker=?`, r.id, old); err != nil {
			return err
		}
	}
	// Port links: the workers' IDs next to their (certificate) names.
	lrs, err := tx.Query(`SELECT id, data FROM port_links`)
	if err != nil {
		return err
	}
	links := map[string]map[string]any{}
	for lrs.Next() {
		var id, d string
		if err := lrs.Scan(&id, &d); err != nil {
			lrs.Close()
			return err
		}
		var m map[string]any
		if json.Unmarshal([]byte(d), &m) == nil {
			links[id] = m
		}
	}
	lrs.Close()
	if err := lrs.Err(); err != nil {
		return err
	}
	for id, m := range links {
		for _, side := range []string{"from", "to"} {
			name, _ := m[side].(string)
			if r := byName[name]; r != nil {
				m[side+"Id"] = r.id
			}
		}
		b, _ := json.Marshal(m)
		if _, err := tx.Exec(`UPDATE port_links SET data=? WHERE id=?`, string(b), id); err != nil {
			return err
		}
	}
	return nil
}
