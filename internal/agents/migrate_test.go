package agents

import (
	"os"
	"path/filepath"
	"testing"

	"daydayup/internal/store"
)

func TestMigrateLegacyAgentsStore(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(legacy, []byte(`{"bindings":{"dsh":{"agent_id":"dsh","model":"deepseek-r1","mode":"auto"}},"profiles":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := MigrateDB(db, legacy); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	b := st.Binding("dsh")
	if b == nil || b.Model != "deepseek-r1" {
		t.Fatalf("legacy binding lost: %+v", b)
	}
}
