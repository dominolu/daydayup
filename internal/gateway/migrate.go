package gateway

import (
	"fmt"

	"daydayup/internal/accounts"
	"daydayup/internal/agents"
	"daydayup/internal/config"
	"daydayup/internal/pool"
	"daydayup/internal/redact"
	"daydayup/internal/sessions"
	"daydayup/internal/store"
)

// migrateLegacyData imports any pre-database storage artifacts (accounts.enc,
// state.json, the sessions directory, redaction.map.enc, agents.json) into the
// shared SQLite database. Each domain migrates once (idempotent markers); the
// legacy files are never modified. The legacy paths come from the config's
// retained fields; they are migration inputs only.
//
// The database handle is the process-wide shared handle (store.Shared) and is
// deliberately never closed here: closing the last connection to the
// multi-gigabyte gateway database during boot triggers SQLite's blocking WAL
// checkpoint (measured ~16s on a 3.8GB database), the dominant startup cost.
// The shared handle lives for the whole process; the WAL is drained by
// pool.Checkpoint during runtime instead.
func migrateLegacyData(c config.Config) error {
	db, err := store.Shared(c.DBFile)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	if err := accounts.MigrateDB(db, c.AccountStoreFile, c.AccountStoreKeyEnv); err != nil {
		return fmt.Errorf("accounts migration: %w", err)
	}
	if err := pool.MigrateDB(db, c.StateFile); err != nil {
		return fmt.Errorf("pool state migration: %w", err)
	}
	if err := sessions.MigrateDB(db, c.SessionStoreDir, c.SessionStoreKeyEnv); err != nil {
		return fmt.Errorf("sessions migration: %w", err)
	}
	redactFile, redactKey := "", ""
	if c.Redaction != nil {
		redactFile, redactKey = c.Redaction.StoreFile, c.Redaction.StoreKeyEnv
	}
	if err := redact.MigrateDB(db, redactFile, redactKey); err != nil {
		return fmt.Errorf("redaction migration: %w", err)
	}
	if err := agents.MigrateDB(db, c.AgentStoreFile); err != nil {
		return fmt.Errorf("agents migration: %w", err)
	}
	return nil
}
