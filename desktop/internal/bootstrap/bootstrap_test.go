package bootstrap

import (
	"path/filepath"
	"testing"

	"daydayup/internal/config"
)

func TestWriteDefaultConfigLoadsWithEmptyAccounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeDefaultConfig(path, dir); err != nil {
		t.Fatalf("writeDefaultConfig: %v", err)
	}
	t.Setenv("GATEWAY_ADMIN_KEY", "admin-key-0123456789")
	t.Setenv("GATEWAY_API_KEY", "gateway-key-0123456789")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.DefaultProvider != "sensenova" {
		t.Fatalf("default provider = %q, want sensenova", cfg.DefaultProvider)
	}
	if cfg.DBFile != filepath.Join(dir, "gateway.db") {
		t.Fatalf("db file = %q", cfg.DBFile)
	}
	if cfg.Redaction == nil || !cfg.Redaction.Enabled {
		t.Fatal("redaction should be enabled for a fresh desktop profile")
	}
	if cfg.Redaction.StoreFile != filepath.Join(dir, "redaction.map.enc") {
		t.Fatalf("redaction store = %q", cfg.Redaction.StoreFile)
	}
	if len(cfg.Accounts) != 0 {
		t.Fatalf("expected empty account pool, got %d", len(cfg.Accounts))
	}
}

func TestNormalizePaths(t *testing.T) {
	cfg := config.Config{
		DBFile:           "data/gateway.db",
		SessionStoreDir:  "data/sessions",
		AccountStoreFile: "data/accounts.enc",
		AgentStoreFile:   "data/agents.json",
		StateFile:        "data/state.json",
	}
	got := normalizePaths(cfg, "/tmp/daydayup")
	if got.DBFile != "/tmp/daydayup/data/gateway.db" {
		t.Fatalf("db = %q", got.DBFile)
	}
	if got.AccountStoreFile != "/tmp/daydayup/data/accounts.enc" {
		t.Fatalf("accounts = %q", got.AccountStoreFile)
	}
	if got.AgentStoreFile != "/tmp/daydayup/data/agents.json" {
		t.Fatalf("agents = %q", got.AgentStoreFile)
	}
	if got.StateFile != "/tmp/daydayup/data/state.json" {
		t.Fatalf("state = %q", got.StateFile)
	}
}
