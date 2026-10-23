package pool

import (
	"testing"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/provider"
)

// A platform's upstream id can be renamed to a canonical id that another
// platform advertises natively; routing must then aggregate both accounts
// under the canonical name, and the model directory must list it once.
func TestModelRenameAggregatesCanonicalName(t *testing.T) {
	now := time.Now()
	p := New([]config.Account{
		{ID: "sense", Provider: "sensenova", MaxInFlight: 1},
		{ID: "plan", Provider: "tokenplan", MaxInFlight: 1},
	})
	p.SetModelRename(map[string]map[string]string{
		"sensenova": {"deepseek-flash": "deepseek-v4.1-flash-1017"},
	})
	p.mu.Lock()
	p.entries[0].state.Models = []provider.Model{{"id": "deepseek-flash"}, {"id": "kimi-k3"}}
	p.entries[0].state.ModelsCheckedAt = &now
	p.entries[1].state.Models = []provider.Model{{"id": "deepseek-v4.1-flash-1017"}}
	p.entries[1].state.ModelsCheckedAt = &now
	p.mu.Unlock()

	// The canonical name is served by both platforms.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		a, err := p.Acquire("", "deepseek-v4.1-flash-1017", excluded(seen))
		if err != nil {
			t.Fatalf("canonical model was not routable across platforms: %v", err)
		}
		seen[a.ID] = true
		p.Finish(a.ID, 200, Usage{}, false, 0, "deepseek-v4.1-flash-1017")
	}
	if !seen["sense"] || !seen["plan"] {
		t.Fatalf("canonical model did not aggregate both platforms: %v", seen)
	}

	// The raw sensenova id still resolves to the same canonical model, so a
	// client that has not migrated keeps working.
	a, err := p.Acquire("", "deepseek-flash", nil)
	if err != nil || a.ID != "sense" {
		t.Fatalf("raw renamed id did not route to its platform: %v %v", a.ID, err)
	}
	p.Finish(a.ID, 200, Usage{}, false, 0, "deepseek-flash")

	models, discovered := p.modelList(time.Hour, func(*entry) bool { return true })
	if !discovered {
		t.Fatal("model list was reported as undiscovered")
	}
	found := 0
	for _, m := range models {
		switch m["id"] {
		case "deepseek-v4.1-flash-1017":
			found++
			ps, _ := m["platforms"].([]string)
			if len(ps) != 2 || ps[0] != "sensenova" || ps[1] != "tokenplan" {
				t.Fatalf("canonical model platforms = %v, want both platforms", m["platforms"])
			}
		case "deepseek-flash":
			t.Fatal("renamed upstream id must not be advertised next to its canonical name")
		}
	}
	if found != 1 {
		t.Fatalf("canonical model listed %d times, want exactly one entry: %v", found, models)
	}
}

func excluded(seen map[string]bool) map[string]bool {
	out := map[string]bool{}
	for id := range seen {
		out[id] = true
	}
	return out
}

// A rename must not make an account eligible for a model it never discovered,
// and a denial keyed by the canonical name must exclude the renamed account.
func TestModelRenameRespectsDiscoveryAndDenials(t *testing.T) {
	now := time.Now()
	p := New([]config.Account{{ID: "sense", Provider: "sensenova", MaxInFlight: 1}})
	p.SetModelRename(map[string]map[string]string{"sensenova": {"deepseek-flash": "canonical"}})
	p.mu.Lock()
	p.entries[0].state.Models = []provider.Model{{"id": "deepseek-flash"}}
	p.entries[0].state.ModelsCheckedAt = &now
	p.mu.Unlock()

	if a, err := p.Acquire("", "canonical", nil); err != nil || a.ID != "sense" {
		t.Fatalf("renamed model did not route: %v %v", a.ID, err)
	}
	p.Finish("sense", 200, Usage{}, false, 0, "canonical")
	if _, err := p.Acquire("", "not-discovered", nil); err == nil {
		t.Fatal("an undiscovered model was routed to the account")
	}
	p.DenyModel("sense", "canonical", time.Minute)
	if _, err := p.Acquire("", "canonical", nil); err == nil {
		t.Fatal("canonical model denial did not exclude the renamed account")
	}
}
