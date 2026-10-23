package redact

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"safuno/internal/store"
)

// MigrateDB imports the legacy whole-file redaction mapping store
// (redaction.map.enc, AES encrypted with the old key) into the shared
// database, one encrypted row per scope. Idempotent per marker; a missing
// legacy file is a valid no-op.
func MigrateDB(db *store.DB, legacyPath, legacyKeyEnv string) error {
	if legacyPath == "" {
		return nil
	}
	done, err := db.Migrated("redact_v1")
	if err != nil || done {
		return err
	}
	b, err := os.ReadFile(legacyPath)
	if os.IsNotExist(err) {
		return db.MarkMigrated("redact_v1")
	}
	if err != nil {
		return err
	}
	legacy, err := legacyRedactAEAD(legacyPath, legacyKeyEnv)
	if err != nil {
		return fmt.Errorf("redact migration: %w", err)
	}
	if len(b) < legacy.NonceSize()+legacy.Overhead() {
		return errors.New("redact migration: truncated legacy store")
	}
	plain, err := legacy.Open(nil, b[:legacy.NonceSize()], b[legacy.NonceSize():], nil)
	if err != nil {
		return errors.New("redact migration: cannot decrypt legacy store; check the old redaction store key")
	}
	var data struct {
		Version int                              `json:"version"`
		Scopes  map[string]map[string]storeEntry `json:"scopes"`
	}
	if err := json.Unmarshal(plain, &data); err != nil {
		return errors.New("redact migration: invalid legacy payload")
	}
	if data.Version != 1 {
		return fmt.Errorf("redact migration: unsupported legacy version %d", data.Version)
	}
	tx, err := db.SQL().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for scope, entries := range data.Scopes {
		next := map[string]storeEntry{}
		for token, entry := range entries {
			next[token] = storeEntry{Label: entry.Label, Original: entry.Original, Touched: entry.Touched}
		}
		payload, err := json.Marshal(struct {
			Version int                   `json:"version"`
			Entries map[string]storeEntry `json:"entries"`
		}{1, next})
		if err != nil {
			return err
		}
		sealed, err := db.Seal(string(payload), "redact:"+scope)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO redact_rules(scope, rules) VALUES(?, ?) ON CONFLICT(scope) DO UPDATE SET rules = excluded.rules`, scope, sealed); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.MarkMigrated("redact_v1")
}

func legacyRedactAEAD(path, keyEnv string) (cipher.AEAD, error) {
	var key []byte
	var err error
	if keyEnv != "" {
		if value := os.Getenv(keyEnv); value != "" {
			key, err = base64.StdEncoding.DecodeString(value)
			if err != nil || len(key) != 32 {
				return nil, errors.New("legacy redaction store key must be 32 bytes encoded as base64")
			}
		}
	}
	if len(key) != 32 {
		key, err = os.ReadFile(path + ".key")
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("legacy redaction store key missing or invalid (%s)", filepath.Base(path)+".key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
