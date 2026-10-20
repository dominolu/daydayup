package agents

import (
	"encoding/json"
	"fmt"
	"os"

	"daydayup/internal/store"
)

// MigrateDB imports the legacy agents.json (plaintext JSON) into the shared
// database. Idempotent per marker; a missing legacy file is a valid no-op.
func MigrateDB(db *store.DB, legacyPath string) error {
	if legacyPath == "" {
		return nil
	}
	done, err := db.Migrated("agents_v1")
	if err != nil || done {
		return err
	}
	b, err := os.ReadFile(legacyPath)
	if os.IsNotExist(err) {
		return db.MarkMigrated("agents_v1")
	}
	if err != nil {
		return err
	}
	if len(trimSpace(b)) == 0 {
		return db.MarkMigrated("agents_v1")
	}
	var data struct {
		Bindings map[string]*Binding `json:"bindings"`
		Profiles map[string]*Profile `json:"profiles"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return fmt.Errorf("agents migration: legacy store corrupt: %w", err)
	}
	tx, err := db.SQL().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, binding := range data.Bindings {
		if binding == nil {
			continue
		}
		payload, err := json.Marshal(binding)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO agents_bindings(agent_id, binding) VALUES(?, ?) ON CONFLICT(agent_id) DO UPDATE SET binding = excluded.binding`, id, string(payload)); err != nil {
			return err
		}
	}
	for name, profile := range data.Profiles {
		if profile == nil {
			continue
		}
		payload, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO agents_profiles(name, profile) VALUES(?, ?) ON CONFLICT(name) DO UPDATE SET profile = excluded.profile`, name, string(payload)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.MarkMigrated("agents_v1")
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
