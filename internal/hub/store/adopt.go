package store

import (
	"encoding/json"
	"errors"
	"time"
)

// QueueAdoption records a one-time adoption for the running hub (or its next
// start). It never replaces node rows held in memory by another process.
func (s *Store) QueueAdoption(tenants []Tenant, plan string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT (SELECT count(*) FROM tenants) + (SELECT count(*) FROM meta WHERE k='adoption')`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("adopt requires a hub without users and without a previous adoption")
	}
	for _, t := range tenants {
		if _, err := tx.Exec(`INSERT INTO tenants(name,data) VALUES(?,?)`, t.Name, string(t.Data)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(k,v) VALUES('adoption',?)`, plan); err != nil {
		return err
	}
	now := time.Now().UTC()
	line, err := json.Marshal(map[string]any{"time": now.Format(time.RFC3339Nano), "who": "local",
		"method": "workers/adopt", "assignments": json.RawMessage(plan)})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO audit(node,time,who,method,line,attributed) VALUES('hub',?,'local','workers/adopt',?,1)`,
		now.UnixMilli(), string(line)); err != nil {
		return err
	}
	return tx.Commit()
}
