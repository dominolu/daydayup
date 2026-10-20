package accounts

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"daydayup/internal/config"
)

func TestEncryptedRoundTripAndTombstones(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := config.Account{ID: "ui-account", Name: "测试账号", Provider: "sensenova", BaseURL: "https://token.sensenova.cn", APIKey: "credential-that-must-not-be-plaintext", MaxInFlight: 4}
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(a.APIKey)) || bytes.Contains(raw, []byte(a.Name)) {
		t.Fatal("plaintext account material persisted")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("database file is not private: %v", info.Mode().Perm())
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reloaded.Load(nil)
	if err != nil || len(loaded) != 1 || loaded[0].APIKey != a.APIKey || !loaded[0].Managed {
		t.Fatalf("round trip: %+v %v", loaded, err)
	}
	if err = s.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = reloaded.Load([]config.Account{a})
	if err != nil || len(loaded) != 0 {
		t.Fatal("deleted configured account resurrected")
	}
}

func TestEnvironmentCredentialsRemainReferences(t *testing.T) {
	t.Setenv("TEST_STORE_ENV_KEY", "original-test-key")
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := config.Account{ID: "env", Provider: "sensenova", BaseURL: "https://token.sensenova.cn", APIKeyEnv: "TEST_STORE_ENV_KEY", APIKey: "original-test-key", MaxInFlight: 4}
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_STORE_ENV_KEY", "rotated-test-key")
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reloaded.Load(nil)
	if err != nil || loaded[0].APIKey != "rotated-test-key" {
		t.Fatal("environment credential was copied instead of referenced")
	}
}

func TestCorruptionAndWrongMasterKeyFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveAccount(config.Account{ID: "a", APIKey: "secret"}); err != nil {
		t.Fatal(err)
	}
	// Tamper a stored account row: the outer AEAD must reject it on load.
	const tamperSQL = "UPDATE accounts SET record = 'tampered' WHERE id = 'a'"
	if _, err := s.db.SQL().Exec(tamperSQL); err != nil {
		t.Fatal(err)
	}
	tampered, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tampered.Load(nil); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	t.Setenv("GATEWAY_DB_KEY", "not-base64")
	if _, err := New(filepath.Join(t.TempDir(), "other.db")); err == nil {
		t.Fatal("bad database key was accepted")
	}
}
