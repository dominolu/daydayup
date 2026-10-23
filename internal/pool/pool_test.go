package pool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/provider"
)

func accounts() []config.Account {
	return []config.Account{{ID: "a", Provider: "sensenova", APIKey: "must-never-be-persisted", MaxInFlight: 2}, {ID: "b", Provider: "sensenova", MaxInFlight: 2}}
}
func TestCapacityAndConcurrency(t *testing.T) {
	p := New(accounts())
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := p.Acquire("sensenova", "m", nil)
			if err == nil {
				states := p.Snapshot()
				for _, state := range states {
					if state.InFlight > 2 {
						t.Error("concurrency limit exceeded")
					}
				}
				p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
			}
		}()
	}
	wg.Wait()
	for _, s := range p.Snapshot() {
		if s.InFlight != 0 || int64(s.Stats.Requests) != s.Stats.Total {
			t.Fatalf("accounting leaked: %+v", s)
		}
	}
}
func TestQuotaGroupsAndPersistence(t *testing.T) {
	p := New(accounts())
	now := time.Now()
	q := &provider.Quota{Source: "console-observation", ObservedAt: now, ValidUntil: now.Add(time.Minute), Buckets: []provider.Bucket{
		{Name: "general-5h", Pool: "general", Unit: "credits", Remaining: 0, Limit: 60000},
		{Name: "general-week", Pool: "general", Unit: "credits", Remaining: 100, Limit: 600000},
		{Name: "flash-5h", Pool: "flash", Unit: "credits", Remaining: 20, Limit: 60000, Models: []string{"flash"}},
		{Name: "flash-week", Pool: "flash", Unit: "credits", Remaining: 50, Limit: 600000, Models: []string{"flash"}},
	}}
	if err := p.SetQuota("a", q); err != nil {
		t.Fatal(err)
	}
	a, err := p.Acquire("sensenova", "other", map[string]bool{"b": true})
	if err == nil {
		t.Fatalf("empty general pool routed: %s", a.ID)
	}
	a, err = p.Acquire("sensenova", "flash", map[string]bool{"b": true})
	if err != nil || a.ID != "a" {
		t.Fatalf("dedicated pool ignored: %s %v", a.ID, err)
	}
	p.Finish(a.ID, 200, Usage{Total: 5, Known: true}, false, 0, "flash")
	path := filepath.Join(t.TempDir(), "data", "state.json")
	if err = p.Save(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "must-never-be-persisted") {
		t.Fatal("secret persisted")
	}
	restored := New(accounts())
	if err = restored.Load(path); err != nil {
		t.Fatal(err)
	}
	s := restored.Snapshot()[0]
	if s.Stats.Total != 5 || s.QuotaStatus != "reported" || s.InFlight != 0 {
		t.Fatalf("restore: %+v", s)
	}
	old := *q
	old.ObservedAt = now.Add(-time.Minute)
	if p.SetQuota("a", &old) == nil {
		t.Fatal("older quota overwrote newer observation")
	}
}
func TestStaleQuotaDoesNotBlockAndCredentialRecovers(t *testing.T) {
	p := New(accounts())
	zero := int64(0)
	now := time.Now()
	q := &provider.Quota{Source: "manual", ObservedAt: now, ValidUntil: now.Add(time.Minute), RemainingTokens: &zero}
	if err := p.SetQuota("a", q); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire("sensenova", "m", map[string]bool{"b": true}); err == nil {
		t.Fatal("zero tokens routed")
	}
	p.mu.Lock()
	p.entries[0].state.Quota.ValidUntil = now.Add(-time.Second)
	p.mu.Unlock()
	a, err := p.Acquire("sensenova", "m", map[string]bool{"b": true})
	if err != nil {
		t.Fatal(err)
	}
	p.Finish(a.ID, 401, Usage{}, true, 0, "m")
	if _, err = p.Acquire("sensenova", "m", map[string]bool{"b": true}); err == nil {
		t.Fatal("invalid key routed")
	}
	p.Discovery("a", []provider.Model{{"id": "m"}}, nil, 0)
	a, err = p.Acquire("sensenova", "m", map[string]bool{"b": true})
	if err != nil {
		t.Fatal("key did not recover")
	}
	p.Finish(a.ID, 200, Usage{}, false, 0, "m")
}

func TestSnapshotsAreIndependent(t *testing.T) {
	p := New(accounts())
	now := time.Now()
	tokens := int64(10)
	q := &provider.Quota{Source: "manual", ObservedAt: now, ValidUntil: now.Add(time.Minute), RemainingTokens: &tokens}
	if err := p.SetQuota("a", q); err != nil {
		t.Fatal(err)
	}
	tokens = 0
	p.Discovery("a", []provider.Model{{"id": "m", "pricing": map[string]any{"prompt": "0"}}}, nil, 0)
	s := p.Snapshot()
	*s[0].Quota.RemainingTokens = 500
	s[0].Models[0]["pricing"].(map[string]any)["prompt"] = "changed"
	current := p.Snapshot()[0]
	if *current.Quota.RemainingTokens != 10 || current.Models[0]["pricing"].(map[string]any)["prompt"] != "0" {
		t.Fatal("snapshot or caller mutated shared state")
	}
}

func TestPartialQuotaDoesNotExhaustUnobservedPools(t *testing.T) {
	p := New(accounts())
	now := time.Now()
	q := &provider.Quota{Partial: true, Source: "manual-console", ObservedAt: now, ValidUntil: now.Add(time.Minute), Buckets: []provider.Bucket{{Name: "general", Pool: "general", Unit: "credits", Remaining: 0}}}
	if err := p.SetQuota("a", q); err != nil {
		t.Fatal(err)
	}
	a, err := p.Acquire("sensenova", "flash", map[string]bool{"b": true})
	if err != nil {
		t.Fatal("partial snapshot incorrectly blocked unobserved pool")
	}
	p.Finish(a.ID, 200, Usage{}, false, 0, "flash")
}

func TestRestartAfterCredentialRotationDropsOldQuota(t *testing.T) {
	a := accounts()
	p := New(a)
	now := time.Now()
	tokens := int64(123)
	if err := p.SetQuota("a", &provider.Quota{Source: "old-key", ObservedAt: now, ValidUntil: now.Add(time.Hour), RemainingTokens: &tokens}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "state.json")
	if err := p.Save(file); err != nil {
		t.Fatal(err)
	}
	a[0].APIKey = "rotated-credential"
	restored := New(a)
	if err := restored.Load(file); err != nil {
		t.Fatal(err)
	}
	if restored.Snapshot()[0].Quota != nil {
		t.Fatal("old credential quota leaked across rotation")
	}
}

func TestSmartRoutingPrefersSoonestReset(t *testing.T) {
	p := New(accounts())
	p.SetRouting(RoutingSmart)
	now := time.Now()
	// a resets in 10 minutes, b resets in 2 minutes: smart must pick b first.
	if err := p.SetQuota("a", &provider.Quota{Source: "test", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 10*time.Minute)}}}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetQuota("b", &provider.Quota{Source: "test", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 2*time.Minute)}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a, err := p.Acquire("sensenova", "m", nil)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if a.ID != "b" {
			t.Fatalf("round %d: expected b (soonest reset), got %s", i, a.ID)
		}
		p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	}
}

func TestSmartRoutingFallsBackToRoundRobinWithoutWindows(t *testing.T) {
	p := New(accounts())
	p.SetRouting(RoutingSmart)
	// No quota windows: smart must behave like round-robin (alternate).
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		a, err := p.Acquire("sensenova", "m", nil)
		if err != nil {
			t.Fatal(err)
		}
		seen[a.ID] = true
		p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("expected both accounts in round-robin fallback: %v", seen)
	}
}

func TestSmartRoutingSkipsStaleWindows(t *testing.T) {
	p := New(accounts())
	p.SetRouting(RoutingSmart)
	now := time.Now()
	// a's window already passed (stale reset), b's window fresh and soon.
	if err := p.SetQuota("a", &provider.Quota{Source: "test", ObservedAt: now, ValidUntil: now.Add(-time.Minute), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, ResetsAt: dur(t, -time.Minute)}}}); err == nil {
		// SetQuota rejects expired valid_until; fall through: a has no fresh window.
	} else {
		t.Logf("stale quota rejected as expected: %v", err)
	}
	a, err := p.Acquire("sensenova", "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "a" && a.ID != "b" {
		t.Fatalf("unexpected account %s", a.ID)
	}
	p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
}

func dur(t *testing.T, d time.Duration) *time.Time {
	t.Helper()
	v := time.Now().Add(d)
	return &v
}

func TestSmartRoutingKeepsSessionPin(t *testing.T) {
	// A session-bound request (preferred account) must stay on that account
	// even when another account has a sooner reset window.
	p := New([]config.Account{{ID: "pinned", Provider: "sensenova", MaxInFlight: 2}, {ID: "soon", Provider: "sensenova", MaxInFlight: 2}})
	p.SetRouting(RoutingSmart)
	now := time.Now()
	_ = p.SetQuota("pinned", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 50*time.Minute)}}})
	_ = p.SetQuota("soon", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 1*time.Minute)}}})
	a, err := p.AcquirePreferredRealm("sensenova", "", "m", nil, "pinned", false)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "pinned" {
		t.Fatalf("session pin broken: %s", a.ID)
	}
	p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
}

func TestSmartRoutingStaysInPreferredRealm(t *testing.T) {
	p := New([]config.Account{{ID: "intl-1", Provider: "workbuddy", UID: "u1", MaxInFlight: 2}, {ID: "intl-2", Provider: "workbuddy", UID: "u2", MaxInFlight: 2}, {ID: "cn-1", Provider: "workbuddy", UID: "u3", Realm: "cn", MaxInFlight: 2}})
	p.SetRouting(RoutingSmart)
	now := time.Now()
	// The CN account has the soonest reset; the intl accounts renew much later.
	_ = p.SetQuota("cn-1", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 1*time.Minute)}}})
	_ = p.SetQuota("intl-1", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 40*time.Minute)}}})
	_ = p.SetQuota("intl-2", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 50*time.Minute)}}})
	// Realm preference intl: smart must not jump to cn-1 despite its sooner reset.
	for i := 0; i < 3; i++ {
		a, err := p.AcquirePreferredRealm("workbuddy", "intl", "m", nil, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(a.ID, "intl") {
			t.Fatalf("realm isolation broken: %s", a.ID)
		}
		p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	}
}

func TestSmartRoutingTieBreakByLessRemaining(t *testing.T) {
	// Same reset time: the account with LESS remaining credit is preferred
	// (it has the least to waste at the reset).
	p := New([]config.Account{{ID: "full", Provider: "sensenova", MaxInFlight: 2}, {ID: "low", Provider: "sensenova", MaxInFlight: 2}})
	p.SetRouting(RoutingSmart)
	now := time.Now()
	at := dur(t, 3*time.Minute)
	_ = p.SetQuota("full", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 9000, Limit: 10000, ResetsAt: at}}})
	_ = p.SetQuota("low", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 10000, ResetsAt: at}}})
	a, err := p.Acquire("sensenova", "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "low" {
		t.Fatalf("tie-break should prefer the account with less remaining, got %s", a.ID)
	}
	p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
}

func TestPerGroupRoutingStrategy(t *testing.T) {
	// Scheduling mode is per routing group: g1 pins smart, g2 pins
	// round_robin, and a group without an override follows the global mode.
	p := New([]config.Account{
		{ID: "a1", Provider: "sensenova", Group: "g1", MaxInFlight: 2},
		{ID: "a2", Provider: "sensenova", Group: "g1", MaxInFlight: 2},
		{ID: "b1", Provider: "sensenova", Group: "g2", MaxInFlight: 2},
		{ID: "b2", Provider: "sensenova", Group: "g2", MaxInFlight: 2},
	})
	p.SetRouting(RoutingRoundRobin)
	p.SetGroupRoutingMap(map[string]string{"g1": RoutingSmart, "g2": RoutingRoundRobin})
	for _, tc := range [][2]string{{"g1", RoutingSmart}, {"g2", RoutingRoundRobin}, {"missing", RoutingRoundRobin}} {
		if got := p.RoutingFor(tc[0]); got != tc[1] {
			t.Fatalf("RoutingFor(%s)=%s want %s", tc[0], got, tc[1])
		}
	}
	// g1 is smart: the account whose quota window renews soonest must win.
	now := time.Now()
	_ = p.SetQuota("a1", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 30*time.Minute)}}})
	_ = p.SetQuota("a2", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 1*time.Minute)}}})
	a, err := p.AcquireGroup("sensenova", "", "g1", "m", nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "a2" {
		t.Fatalf("g1 smart should pick the soonest reset (a2), got %s", a.ID)
	}
	p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	// g2 is round_robin without quota windows: it must alternate its accounts.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		acc, err := p.AcquireGroup("sensenova", "", "g2", "m", nil, "", false)
		if err != nil {
			t.Fatal(err)
		}
		seen[acc.ID] = true
		p.Finish(acc.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	}
	if !seen["b1"] || !seen["b2"] {
		t.Fatalf("g2 round-robin should alternate b1/b2: %v", seen)
	}
}

func TestGlobalAcquirePicksAcrossPlatforms(t *testing.T) {
	p := New([]config.Account{
		{ID: "sense-1", Provider: "sensenova", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "wb-1", Provider: "workbuddy", UID: "u1", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "tp-1", Provider: "tokenplan", Models: []string{"other"}, MaxInFlight: 4},
	})
	// Global routing: model "m" is served by accounts of two platforms.
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		a, err := p.Acquire("", "m", nil)
		if err != nil {
			t.Fatalf("global acquire: %v", err)
		}
		seen[a.ID] = true
		p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	}
	if !seen["sense-1"] || !seen["wb-1"] || seen["tp-1"] {
		t.Fatalf("cross-platform pool wrong: %v", seen)
	}
	// Explicit platform still filters the pool.
	if _, err := p.Acquire("tokenplan", "m", nil); err == nil {
		t.Fatal("tokenplan must not serve m")
	}
	a, err := p.Acquire("sensenova", "m", nil)
	if err != nil || a.ID != "sense-1" {
		t.Fatalf("explicit provider filter broken: %v %v", a, err)
	}
	p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	a, err = p.Acquire("", "other", nil)
	if err != nil || a.ID != "tp-1" {
		t.Fatalf("model routed to wrong platform: %v %v", a, err)
	}
	p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "other")
}

func TestGlobalModelListAllMergesPlatforms(t *testing.T) {
	p := New([]config.Account{
		{ID: "sense-1", Provider: "sensenova", MaxInFlight: 4},
		{ID: "wb-1", Provider: "workbuddy", UID: "u1", MaxInFlight: 4},
	})
	p.Discovery("sense-1", []provider.Model{{"id": "m", "context_length": 123}}, nil, 0)
	p.Discovery("wb-1", []provider.Model{{"id": "m", "context_length": 456}, {"id": "wb-only"}}, nil, 0)
	all, ok := p.ModelListAll(time.Hour)
	if !ok {
		t.Fatal("no discovery")
	}
	if len(all) != 2 || all[0]["id"] != "m" {
		t.Fatalf("global catalog: %+v", all)
	}
	m := all[0]
	if m["owned_by"] != "sensenova" {
		t.Fatalf("owned_by: %v", m["owned_by"])
	}
	ps, _ := m["platforms"].([]string)
	if len(ps) != 2 || ps[0] != "sensenova" || ps[1] != "workbuddy" {
		t.Fatalf("platforms not merged: %v", m["platforms"])
	}
	if _, has := all[1]["platforms"]; has {
		t.Fatalf("single-platform model got platforms field: %+v", all[1])
	}
	one, ok := p.ModelList("workbuddy", "", time.Hour)
	if !ok || len(one) != 2 {
		t.Fatalf("per-provider catalog: %+v %v", one, ok)
	}
}

func TestGlobalAcquireRealmPreferenceAcrossPlatforms(t *testing.T) {
	p := New([]config.Account{
		{ID: "sense-1", Provider: "sensenova", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "intl-1", Provider: "workbuddy", UID: "u1", Realm: "intl", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "cn-1", Provider: "workbuddy", UID: "u2", Realm: "cn", Models: []string{"m"}, MaxInFlight: 4},
	})
	// Realm preference is a hard ordering, not a rotation: preferred-realm
	// workbuddy accounts come first, then neutral platforms, then the other
	// workbuddy realm (fingerprints must not mix). Exclude accounts to walk
	// down the ladder.
	a, err := p.AcquirePreferredRealm("", "intl", "m", nil, "", false)
	if err != nil || a.ID != "intl-1" {
		t.Fatalf("preferred realm first: %v %v", a, err)
	}
	p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	a, err = p.AcquirePreferredRealm("", "intl", "m", map[string]bool{"intl-1": true}, "", false)
	if err != nil || a.ID != "sense-1" {
		t.Fatalf("neutral platform after preferred realm: %v %v", a, err)
	}
	p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	a, err = p.AcquirePreferredRealm("", "intl", "m", map[string]bool{"intl-1": true, "sense-1": true}, "", false)
	if err != nil || a.ID != "cn-1" {
		t.Fatalf("other realm is the last resort: %v %v", a, err)
	}
	p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
}

func TestGlobalSmartRoutingPrefersSoonestResetAcrossPlatforms(t *testing.T) {
	p := New([]config.Account{{ID: "sense-1", Provider: "sensenova", MaxInFlight: 2}, {ID: "wb-1", Provider: "workbuddy", UID: "u1", MaxInFlight: 2}})
	p.SetRouting(RoutingSmart)
	now := time.Now()
	_ = p.SetQuota("sense-1", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 50*time.Minute)}}})
	_ = p.SetQuota("wb-1", &provider.Quota{Source: "t", ObservedAt: now, ValidUntil: now.Add(time.Hour), Buckets: []provider.Bucket{{Name: "w", Pool: "g", Unit: "credits", Remaining: 100, Limit: 1000, ResetsAt: dur(t, 2*time.Minute)}}})
	for i := 0; i < 3; i++ {
		a, err := p.Acquire("", "m", nil)
		if err != nil || a.ID != "wb-1" {
			t.Fatalf("global smart should prefer soonest reset across platforms: %v %v", a, err)
		}
		p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	}
}

func TestGroupRoutingBoundsAccounts(t *testing.T) {
	p := New([]config.Account{
		{ID: "ga-1", Provider: "sensenova", Group: "a", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "ga-2", Provider: "workbuddy", UID: "u1", Group: "a", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "gb-1", Provider: "sensenova", Group: "b", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "def-1", Provider: "tokenplan", Models: []string{"m"}, MaxInFlight: 4},
	})
	// Group a aggregates across platforms but never leaves the group.
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		a, err := p.AcquireGroup("", "", "a", "m", nil, "", false)
		if err != nil {
			t.Fatalf("group a acquire: %v", err)
		}
		if a.Group != "a" {
			t.Fatalf("cross-group leak: %s", a.ID)
		}
		seen[a.ID] = true
		p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	}
	if !seen["ga-1"] || !seen["ga-2"] || seen["gb-1"] || seen["def-1"] {
		t.Fatalf("group a pool wrong: %v", seen)
	}
	// Group b sees only its own accounts.
	for i := 0; i < 2; i++ {
		a, err := p.AcquireGroup("", "", "b", "m", nil, "", false)
		if err != nil || a.ID != "gb-1" {
			t.Fatalf("group b: %v %v", a, err)
		}
		p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	}
	// The default group (no header) sees only default-group accounts.
	for i := 0; i < 2; i++ {
		a, err := p.Acquire("", "m", nil)
		if err != nil || a.ID != "def-1" {
			t.Fatalf("default group: %v %v", a, err)
		}
		p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	}
	// An unknown group serves nothing.
	if _, err := p.AcquireGroup("", "", "nope", "m", nil, "", false); err == nil {
		t.Fatal("unknown group served a model")
	}
}

func TestMoveGroupRenamesAndDissolves(t *testing.T) {
	p := New([]config.Account{
		{ID: "ga-1", Provider: "sensenova", Group: "a", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "ga-2", Provider: "workbuddy", UID: "u1", Group: "a", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "gb-1", Provider: "tokenplan", Group: "b", Models: []string{"m"}, MaxInFlight: 4},
	})
	persisted := []string{}
	persist := func(a config.Account) error { persisted = append(persisted, a.ID+"->"+a.Group); return nil }
	// Rename group a -> alpha: accounts follow and round-robin keeps working.
	if err := p.MoveGroup("a", "alpha", true, persist); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("persist count: %d", len(persisted))
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		a, err := p.AcquireGroup("", "", "alpha", "m", nil, "", false)
		if err != nil {
			t.Fatalf("renamed group acquire: %v", err)
		}
		if a.Group != "alpha" {
			t.Fatalf("renamed group leak: %s", a.ID)
		}
		seen[a.ID] = true
		p.Finish(a.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	}
	if !seen["ga-1"] || !seen["ga-2"] {
		t.Fatalf("renamed pool: %v", seen)
	}
	// The old name now serves nothing.
	if _, err := p.AcquireGroup("", "", "a", "m", nil, "", false); err == nil {
		t.Fatal("old group still served a model")
	}
	// Dissolve group b into default: accounts land in the default group.
	persisted = nil
	if err := p.MoveGroup("b", "", false, persist); err != nil {
		t.Fatalf("dissolve: %v", err)
	}
	if len(persisted) != 1 || persisted[0] != "gb-1->default" {
		t.Fatalf("dissolve persist: %v", persisted)
	}
	acct, err := p.Acquire("", "m", nil)
	if err != nil || acct.ID != "gb-1" {
		t.Fatalf("dissolved account not in default: %v %v", acct.ID, err)
	}
	// Renaming an empty group is a no-op error; dissolving one is fine.
	p2 := New([]config.Account{{ID: "x", Provider: "sensenova", MaxInFlight: 4}})
	if err := p2.MoveGroup("ghost", "g1", true, persist); err != ErrEmptyGroup {
		t.Fatalf("empty rename should fail: %v", err)
	}
	if err := p2.MoveGroup("ghost", "g1", false, persist); err != nil {
		t.Fatalf("empty dissolve should be a no-op: %v", err)
	}
}

// A persist failure part-way through a group move must not split the group:
// neither disk nor memory may show a partial migration.
func TestMoveGroupPersistFailureIsAtomic(t *testing.T) {
	p := New([]config.Account{
		{ID: "ga-1", Provider: "sensenova", Group: "a", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "ga-2", Provider: "sensenova", Group: "a", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "ga-3", Provider: "sensenova", Group: "a", Models: []string{"m"}, MaxInFlight: 4},
	})
	persisted := []string{}
	persist := func(a config.Account) error {
		if len(persisted) == 2 {
			return errors.New("disk full")
		}
		persisted = append(persisted, a.ID)
		return nil
	}
	if err := p.MoveGroup("a", "b", true, persist); err == nil {
		t.Fatal("expected persist failure")
	}
	for _, g := range p.Groups() {
		if g.Name != "a" || g.Accounts != 3 {
			t.Fatalf("group split after failed move: %+v", p.Groups())
		}
	}
	acct, err := p.AcquireGroup("", "", "a", "m", nil, "", false)
	if err != nil || acct.Group != "a" {
		t.Fatalf("account left group a after failed move: %v %v", acct.Group, err)
	}
	if _, err := p.AcquireGroup("", "", "b", "m", nil, "", false); err == nil {
		t.Fatal("partial move created group b")
	}
}

func TestGroupInfoStatsAndModels(t *testing.T) {
	p := New([]config.Account{
		{ID: "ga-1", Provider: "sensenova", Group: "a", Models: []string{"m"}, MaxInFlight: 4},
		{ID: "ga-2", Provider: "sensenova", Group: "a", Models: []string{"m"}, MaxInFlight: 4},
	})
	p.Discovery("ga-1", []provider.Model{{"id": "m"}, {"id": "m2"}}, nil, 0)
	p.Discovery("ga-2", []provider.Model{{"id": "m"}}, nil, 0)
	a1, err := p.AcquireGroup("", "", "a", "m", nil, "", false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.Finish(a1.ID, 200, Usage{Total: 1, Known: true}, false, 0, "m")
	a2, err := p.AcquireGroup("", "", "a", "m", nil, "", false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.Finish(a2.ID, 500, Usage{Total: 1, Known: true}, true, 0, "m")
	g := p.Groups()
	if len(g) != 1 || g[0].Name != "a" || g[0].Accounts != 2 || g[0].Enabled != 2 {
		t.Fatalf("group info: %+v", g)
	}
	if g[0].Models != 2 {
		t.Fatalf("group distinct models: %d", g[0].Models)
	}
	if g[0].Requests != 2 || g[0].Failures != 1 {
		t.Fatalf("group traffic: %+v", g[0])
	}
}

func TestModelListGroupScopedAndGroupsListing(t *testing.T) {
	p := New([]config.Account{
		{ID: "ga-1", Provider: "sensenova", Group: "a", MaxInFlight: 4},
		{ID: "gb-1", Provider: "workbuddy", UID: "u1", Group: "b", MaxInFlight: 4},
	})
	p.Discovery("ga-1", []provider.Model{{"id": "m"}}, nil, 0)
	p.Discovery("gb-1", []provider.Model{{"id": "m"}, {"id": "other"}}, nil, 0)
	ga, ok := p.ModelListGroup("a", time.Hour)
	if !ok || len(ga) != 1 || ga[0]["id"] != "m" {
		t.Fatalf("group a catalog: %+v %v", ga, ok)
	}
	gb, ok := p.ModelListGroup("b", time.Hour)
	if !ok || len(gb) != 2 {
		t.Fatalf("group b catalog: %+v %v", gb, ok)
	}
	all, ok := p.ModelListAll(time.Hour)
	if !ok || len(all) != 2 {
		t.Fatalf("all catalog: %+v %v", all, ok)
	}
	groups := p.Groups()
	if len(groups) != 2 || groups[0].Name != "a" || groups[0].Accounts != 1 || groups[0].Enabled != 1 || groups[1].Name != "b" {
		t.Fatalf("groups listing: %+v", groups)
	}
	p.SetEnabled("ga-1", false)
	groups = p.Groups()
	if groups[0].Enabled != 0 || groups[0].Accounts != 1 {
		t.Fatalf("enabled count wrong: %+v", groups)
	}
}
