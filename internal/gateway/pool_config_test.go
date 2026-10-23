package gateway

import (
	"path/filepath"
	"testing"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/pool"
	"daydayup/internal/provider"
)

// LoadAccounts replaces the account pool. Runtime settings installed at
// construction (guards, routing strategy, per-group routing and model_rename)
// must be reinstalled on the replacement, otherwise the gateway silently runs
// with defaults after boot: "smart" routing degrades to round_robin and
// renamed models disappear from scheduling and /v1/models.
func TestLoadAccountsKeepsRuntimePoolSettings(t *testing.T) {
	c := config.Config{
		DefaultProvider:        "sensenova",
		GatewayKey:             "gateway-secret-at-least-16",
		AdminKey:               "admin-secret-at-least-16",
		RequestTimeoutSeconds:  5,
		RefreshIntervalSeconds: 60,
		MaxRequestBytes:        1 << 20,
		MaxAttempts:            3,
		DBFile:                 filepath.Join(t.TempDir(), "gateway.db"),
		Routing:                "smart",
		GroupRouting:           map[string]string{"worker": "round_robin"},
		ModelRename: map[string]map[string]string{
			"sensenova": {"deepseek-v4.1-flash": "deepseek-v4.1-flash-1017"},
		},
		Accounts: []config.Account{
			{ID: "sense-1", Provider: "sensenova", BaseURL: "https://sensenova.invalid", APIKey: "sense-key-1", MaxInFlight: 2},
			{ID: "plan-1", Provider: "tokenplan", BaseURL: "https://tokenplan.invalid", APIKey: "plan-key-1", MaxInFlight: 2},
		},
	}
	s := New(c)
	if err := s.LoadAccounts(); err != nil {
		t.Fatal(err)
	}
	if got := s.Pool.Routing(); got != "smart" {
		t.Fatalf("routing strategy lost after LoadAccounts: %q", got)
	}
	if got := s.Pool.RoutingFor("worker"); got != "round_robin" {
		t.Fatalf("per-group routing lost after LoadAccounts: %q", got)
	}
	for _, a := range s.Pool.Accounts() {
		switch a.Provider {
		case "sensenova":
			s.Pool.Discovery(a.ID, []provider.Model{{"id": "deepseek-v4.1-flash"}}, nil, 0)
		case "tokenplan":
			s.Pool.Discovery(a.ID, []provider.Model{{"id": "deepseek-v4.1-flash-1017"}}, nil, 0)
		}
	}
	models, ok := s.Pool.ModelListAll(time.Minute)
	if !ok {
		t.Fatal("no discovered models after seeding")
	}
	var entry provider.Model
	for _, m := range models {
		if m["id"] == "deepseek-v4.1-flash-1017" {
			entry = m
		}
		if m["id"] == "deepseek-v4.1-flash" {
			t.Fatal("renamed upstream id advertised next to its canonical name")
		}
	}
	if entry == nil {
		t.Fatalf("canonical model missing after LoadAccounts: %v", models)
	}
	platforms, _ := entry["platforms"].([]string)
	if len(platforms) != 2 || platforms[0] != "sensenova" || platforms[1] != "tokenplan" {
		t.Fatalf("canonical model lost its cross-platform aggregation: %v", entry["platforms"])
	}
	// Scheduling must resolve the canonical name through the rename table on
	// the platform whose account advertises the raw id.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		a, err := s.Pool.Acquire("", "deepseek-v4.1-flash-1017", seen)
		if err != nil {
			t.Fatalf("canonical model not schedulable: %v", err)
		}
		seen[a.ID] = true
		s.Pool.Finish(a.ID, 200, pool.Usage{}, false, 0, "deepseek-v4.1-flash-1017")
	}
	if !seen["sense-1"] || !seen["plan-1"] {
		t.Fatalf("canonical model did not reach both platforms: %v", seen)
	}
}
