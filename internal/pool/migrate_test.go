package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"daydayup/internal/config"
	"daydayup/internal/store"
)

func TestMigrateLegacyState(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "state.json")
	payload, _ := json.Marshal(struct {
		Version  int              `json:"version"`
		Accounts []persistedState `json:"accounts"`
	}{1, []persistedState{
		{State: State{ID: "a", Provider: "sensenova", Stats: Stats{Requests: 7, Total: 42}}},
		{State: State{ID: "missing", Provider: "sensenova", Stats: Stats{Requests: 1}}},
	}})
	if err := os.WriteFile(legacy, payload, 0600); err != nil {
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
	if err := MigrateDB(db, legacy); err != nil {
		t.Fatalf("migration not idempotent: %v", err)
	}
	p := New([]config.Account{{ID: "a", Provider: "sensenova"}, {ID: "b", Provider: "sensenova"}})
	if err := p.Load(dbPath); err != nil {
		t.Fatal(err)
	}
	states := map[string]State{}
	for _, st := range p.Snapshot() {
		states[st.ID] = st
	}
	if states["a"].Stats.Requests != 7 || states["a"].Stats.Total != 42 {
		t.Fatalf("legacy state lost: %+v", states["a"].Stats)
	}
	if states["b"].Stats.Requests != 0 {
		t.Fatal("stray state applied to unknown account")
	}
}
