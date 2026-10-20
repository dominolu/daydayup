package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"daydayup/internal/config"
	"daydayup/internal/store"
)

type persistedState struct {
	State
	CredentialRevision string `json:"credential_revision"`
}

func credentialRevision(a config.Account) string {
	sum := sha256.Sum256([]byte(a.Provider + "\x00" + a.BaseURL + "\x00" + a.APIKey + "\x00" + a.RefreshToken + "\x00" + a.UID))
	return hex.EncodeToString(sum[:])
}

// db lazily opens (once) the shared gateway database for pool state. The same
// database file backs every store; the handle is cached for the process.
func (p *Pool) db(path string) (*store.DB, error) {
	p.persistMu.Lock()
	defer p.persistMu.Unlock()
	if p.persistDB == nil {
		db, err := store.Open(path)
		if err != nil {
			return nil, err
		}
		p.persistDB = db
	}
	return p.persistDB, nil
}

// Save checkpoints every account's scheduling state into the database inside
// one transaction. path is the shared gateway database file.
func (p *Pool) Save(path string) error {
	if path == "" {
		return nil
	}
	db, err := p.db(path)
	if err != nil {
		return err
	}
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	states := make([]persistedState, 0, len(p.entries))
	for _, e := range p.entries {
		states = append(states, persistedState{State: e.state, CredentialRevision: credentialRevision(e.config)})
	}
	p.mu.Unlock()
	tx, err := db.SQL().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range states {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO pool_state(id, state) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET state = excluded.state`, s.ID, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Load restores scheduling state from the database. A missing row set or
// database is valid (fresh start).
func (p *Pool) Load(path string) error {
	if path == "" {
		return nil
	}
	db, err := p.db(path)
	if err != nil {
		return err
	}
	rows, err := db.SQL().Query(`SELECT id, state FROM pool_state`)
	if err != nil {
		return err
	}
	defer rows.Close()
	states := []persistedState{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			return err
		}
		var s persistedState
		if err := json.Unmarshal([]byte(state), &s); err != nil {
			return fmt.Errorf("pool state: invalid row %s", id)
		}
		states = append(states, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range states {
		for _, e := range p.entries {
			if e.state.ID == s.ID && e.state.Provider == s.Provider {
				e.state.Stats = s.Stats
				if s.CredentialRevision == credentialRevision(e.config) {
					e.state.Quota = s.Quota
				}
				e.state.Enabled = s.Enabled
				e.state.DailyReset = s.DailyReset
				e.state.DailyTokens = s.DailyTokens
				e.state.DailyCredits = s.DailyCredits
				if e.state.DailyTokens == nil {
					e.state.DailyTokens = map[string]int64{}
				}
			}
		}
	}
	return nil
}

// MigrateDB imports the legacy state.json (plaintext JSON) into the database.
// Idempotent per marker; a missing legacy file is a valid no-op.
func MigrateDB(db *store.DB, legacyPath string) error {
	if legacyPath == "" {
		return nil
	}
	done, err := db.Migrated("pool_v1")
	if err != nil || done {
		return err
	}
	if legacyPath == "" {
		return db.MarkMigrated("pool_v1")
	}
	b, err := os.ReadFile(legacyPath)
	if os.IsNotExist(err) {
		return db.MarkMigrated("pool_v1")
	}
	if err != nil {
		return err
	}
	var data struct {
		Version  int              `json:"version"`
		Accounts []persistedState `json:"accounts"`
	}
	if err = json.Unmarshal(b, &data); err != nil {
		return err
	}
	if data.Version != 1 {
		return fmt.Errorf("pool migration: unsupported legacy state version %d", data.Version)
	}
	tx, err := db.SQL().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range data.Accounts {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO pool_state(id, state) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET state = excluded.state`, s.ID, string(b)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.MarkMigrated("pool_v1")
}
