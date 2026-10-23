package accounts

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"safuno/internal/config"
)

// TestMain mirrors the platform set the binary registers at boot; the account
// store refuses stored accounts whose platform is not registered.
func TestMain(m *testing.M) {
	config.SetRegisteredProviders([]string{"sensenova", "workbuddy", "tokenplan"})
	config.SetEndpoint("sensenova", config.EndpointSpec{Origin: "https://token.sensenova.cn"})
	config.SetEndpoint("workbuddy", config.EndpointSpec{Origin: "https://www.workbuddy.ai"})
	config.SetRealmPlatform("workbuddy", []string{"intl", "cn"}, []string{"desktop", "vscode", "cli"}, nil)
	os.Exit(m.Run())
}

// storeTestKey is a literal credential the store encrypts at rest.
const storeTestKey = "store-test-secret-key-1"

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

func TestUnregisteredPlatformAccountsAreKeptButNotRouted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A platform that the running gateway knows (hot-registered), not a
	// built-in: the declaration comes from the gateway, not from config.
	defer config.SetRegisteredProviders([]string{"sensenova", "workbuddy", "tokenplan"})
	config.SetRegisteredProviders([]string{"sensenova", "workbuddy", "tokenplan", "hotone"})
	config.SetEndpoint("hotone", config.EndpointSpec{Origin: "https://hot.example.com"})
	t.Cleanup(func() { config.SetEndpoint("hotone", config.EndpointSpec{}) })
	put := func(id, platform string) {
		t.Helper()
		account := config.Account{ID: id, Provider: platform}
		account.APIKey = storeTestKey
		if err := s.SaveAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	put("hot", "hotone")
	put("sense", "sensenova")
	loaded, err := s.Load(nil)
	if err != nil || len(loaded) != 2 {
		t.Fatalf("both platforms registered: %v %v", loaded, err)
	}
	// Removing the platform must not make loading fail (a restart has to
	// start), and its account is reported instead of routed.
	config.SetRegisteredProviders([]string{"sensenova", "workbuddy", "tokenplan"})
	var unbound []string
	loaded, err = s.LoadWithUnbound(nil, &unbound)
	if err != nil {
		t.Fatalf("load with an unbound account must succeed: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ID != "sense" {
		t.Fatalf("unbound account still routed: %+v", loaded)
	}
	if len(unbound) != 1 || unbound[0] != "hot" {
		t.Fatalf("unbound accounts: %v", unbound)
	}
	// The stored row survives: re-registering brings the account back.
	config.SetRegisteredProviders([]string{"sensenova", "workbuddy", "tokenplan", "hotone"})
	if loaded, err = s.Load(nil); err != nil || len(loaded) != 2 {
		t.Fatalf("re-registering did not rebind: %v %v", loaded, err)
	}
	for _, a := range loaded {
		if a.ID == "hot" && a.BaseURL != "https://hot.example.com" {
			t.Fatalf("rebound account endpoint = %+v", a)
		}
	}
}
