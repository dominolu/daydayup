package accounts

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"daydayup/internal/store"
)

// MigrateDB imports the legacy whole-file account store (accounts.enc, AES
// encrypted with the old key) into the shared database. It is idempotent per
// marker and a no-op when the legacy file is absent. The legacy file is never
// modified or deleted; the database becomes the source of truth.
func MigrateDB(db *store.DB, legacyPath, legacyKeyEnv string) error {
	if legacyPath == "" {
		return nil
	}
	done, err := db.Migrated("accounts_v1")
	if err != nil || done {
		return err
	}
	b, err := os.ReadFile(legacyPath)
	if os.IsNotExist(err) {
		return db.MarkMigrated("accounts_v1")
	}
	if err != nil {
		return err
	}
	legacy, err := legacyAEAD(legacyPath, legacyKeyEnv)
	if err != nil {
		return fmt.Errorf("accounts migration: %w", err)
	}
	if len(b) < 4+legacy.NonceSize()+legacy.Overhead() || string(b[:4]) != "DDN1" || len(b) > 8<<20 {
		return errors.New("accounts migration: invalid legacy store")
	}
	plain, err := legacy.Open(nil, b[4:4+legacy.NonceSize()], b[4+legacy.NonceSize():], []byte("DDN1"))
	if err != nil {
		return errors.New("accounts migration: cannot decrypt legacy store; check the old account store key")
	}
	var records []record
	if json.Unmarshal(plain, &records) != nil {
		return errors.New("accounts migration: invalid legacy payload")
	}
	tx, err := db.SQL().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range records {
		plain, err := json.Marshal(r)
		if err != nil {
			return err
		}
		sealed, err := db.Seal(string(plain), "account:"+r.Account.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO accounts(id, record) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET record = excluded.record`, r.Account.ID, sealed); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.MarkMigrated("accounts_v1")
}

// legacyAEAD rebuilds the old file-store cipher from its key {{SECRET_rqydrm}} env or
// <path>.key file, so pre-database data can be decrypted once during migration.
func legacyAEAD(path, keyEnv string) (cipher.AEAD, error) {
	var key []byte
	var err error
	if keyEnv != "" {
		if value := os.Getenv(keyEnv); value != "" {
			key, err = base64.StdEncoding.DecodeString(value)
			if err != nil || len(key) != 32 {
				return nil, errors.New("legacy account store key must be 32 bytes encoded as base64")
			}
		}
	}
	if len(key) != 32 {
		key, err = os.ReadFile(path + ".key")
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("legacy account store key missing or invalid (%s)", filepath.Base(path)+".key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
