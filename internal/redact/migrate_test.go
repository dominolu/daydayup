package redact

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"safuno/internal/store"
)

func TestMigrateLegacyRedactionStore(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "redaction.map.enc")
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	_ = os.WriteFile(legacy+".key", key, 0600)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	wantOriginal := "user@example.com"
	legacyToken := "{{" + "EMAIL_" + "bcbcbc" + "}}"
	payload, _ := json.Marshal(struct {
		Version int                              `json:"version"`
		Scopes  map[string]map[string]storeEntry `json:"scopes"`
	}{1, map[string]map[string]storeEntry{"default": {legacyToken: {Label: "EMAIL", Original: wantOriginal}}}})
	sealed := aead.Seal(nonce, nonce, payload, nil)
	if err := os.WriteFile(legacy, sealed, 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := MigrateDB(db, legacy, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := OpenStore(dbPath, "")
	entries, err := st.Load("default")
	if err != nil {
		t.Fatal(err)
	}
	if entries[legacyToken].original != wantOriginal {
		t.Fatalf("legacy mapping lost: %+v", entries)
	}
}
