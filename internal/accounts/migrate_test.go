package accounts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"daydayup/internal/config"
	"daydayup/internal/store"
)

// writeLegacyStore produces an accounts.enc in the pre-database format:
// "DDN1" || AEAD(JSON records), key in accounts.enc.key.
func writeLegacyStore(t *testing.T, path string, records []record) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".key", key, 0600); err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	plain, _ := json.Marshal(records)
	sealed := aead.Seal(nonce, nonce, plain, []byte("DDN1"))
	if err := os.WriteFile(path, append([]byte("DDN1"), sealed...), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateLegacyAccountStore(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "accounts.enc")
	writeLegacyStore(t, legacy, []record{{Account: config.Account{ID: "legacy-1", Provider: "sensenova", BaseURL: "https://token.sensenova.cn"}, APIKey: "secret"}})
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := MigrateDB(db, legacy, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if again := MigrateDB(db, legacy, ""); again != nil {
		t.Fatalf("migration not idempotent: %v", again)
	}
	st, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load(nil)
	if err != nil || len(loaded) != 1 || loaded[0].ID != "legacy-1" || loaded[0].APIKey != "secret" || !loaded[0].Managed {
		t.Fatalf("legacy account lost or mangled: %+v %v", loaded, err)
	}
}
