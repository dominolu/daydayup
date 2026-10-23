package pool

import (
	"testing"
	"time"

	"safuno/internal/config"
	"safuno/internal/provider"
	_ "safuno/internal/provider/testsupport"
)

func TestDailyGuardsBlockAndRoll(t *testing.T) {
	p := New(nil)
	p.SetGuards(config.Guards{DailyTokens: 100, PerModelTokens: 60, DailyCredits: 400, ResetHour: 4})
	a := config.Account{ID: "wb-1", Provider: "workbuddy", APIKey: "tok-1", UID: "u1", BaseURL: "https://www.workbuddy.ai", Realm: "intl", Product: "desktop", MaxInFlight: 8}
	if err := p.Upsert(a, true, func(config.Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Seed model discovery so credit rates are known (gpt-6-astra x6.67).
	p.DiscoveryFor(a, []provider.Model{{"id": "hy3", "credits": "x0.00"}, {"id": "gpt-6-astra", "credits": "x6.67"}}, nil, 0)
	// 30 tokens on the paid model -> 200.1 credits, under the 400 daily cap.
	if _, err := p.AcquirePreferred("workbuddy", "gpt-6-astra", nil, "", false); err != nil {
		t.Fatal(err)
	}
	p.Finish("wb-1", 200, Usage{Input: 10, Output: 20, Total: 30, Known: true}, false, 0, "gpt-6-astra")
	// Another 40 -> gpt total 70 > per-model 60.
	if _, err := p.AcquirePreferred("workbuddy", "gpt-6-astra", nil, "", false); err != nil {
		t.Fatal(err)
	}
	p.Finish("wb-1", 200, Usage{Total: 40, Known: true}, false, 0, "gpt-6-astra")
	// Now the per-model daily cap blocks this model.
	if _, err := p.AcquirePreferred("workbuddy", "gpt-6-astra", nil, "", false); err == nil {
		t.Fatal("per-model daily token guard must block")
	} else if se, ok := err.(*AcquireError); !ok || len(se.Rejections) != 1 || se.Rejections[0].Reason != "per_model_daily_token_limit" {
		t.Fatalf("rejection: %v", err)
	}
	// Free model still works (its credit rate is 0, per-model cap differs).
	if _, err := p.AcquirePreferred("workbuddy", "hy3", nil, "", false); err != nil {
		t.Fatalf("free model must stay schedulable: %v", err)
	}
	p.Finish("wb-1", 200, Usage{Input: 5, Output: 5, Total: 10, Known: true}, false, 0, "hy3")
	// One more free-model call lands the daily total exactly at 100.
	if _, err := p.AcquirePreferred("workbuddy", "hy3", nil, "", false); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	p.Finish("wb-1", 200, Usage{Total: 20, Known: true}, false, 0, "hy3")
	// Daily total = 30+40+10+20 = 100 >= 100: blocked for every model.
	if _, err := p.AcquirePreferred("workbuddy", "hy3", nil, "", false); err == nil {
		t.Fatal("daily token guard must block all models")
	} else if se, ok := err.(*AcquireError); !ok || se.Rejections[0].Reason != "daily_token_limit" {
		t.Fatalf("rejection: %v", err)
	}
	// Roll over: counters belong to a new window key at/after 04:00 next day.
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), 4, 30, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	p.mu.Lock()
	p.entries[0].resetDaily(next, 4)
	p.mu.Unlock()
	if _, err := p.AcquirePreferred("workbuddy", "hy3", nil, "", false); err != nil {
		t.Fatalf("daily rollover must unblock: %v", err)
	}
	p.Finish("wb-1", 200, Usage{Total: 1, Known: true}, false, 0, "hy3")
}

func TestReserveCreditsGuard(t *testing.T) {
	p := New(nil)
	p.SetGuards(config.Guards{ReserveCredits: 100})
	a := config.Account{ID: "wb-1", Provider: "workbuddy", APIKey: "tok-1", UID: "u1", BaseURL: "https://www.workbuddy.ai", Realm: "intl", Product: "desktop", MaxInFlight: 8}
	_ = p.Upsert(a, true, func(config.Account) error { return nil })
	now := time.Now()
	q := &provider.Quota{Source: "test", ObservedAt: now, ValidUntil: now.Add(5 * time.Minute), Buckets: []provider.Bucket{{Name: "p", Pool: "credits", Unit: "credits", Remaining: 120, Limit: 200}}}
	if err := p.SetQuota("wb-1", q); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AcquirePreferred("workbuddy", "hy3", nil, "", false); err != nil {
		t.Fatalf("above reserve must pass: %v", err)
	}
	p.Finish("wb-1", 200, Usage{Total: 1, Known: true}, false, 0, "hy3")
	// Drop below the reserve floor.
	q.Buckets[0].Remaining = 50
	_ = p.SetQuota("wb-1", q)
	if _, err := p.AcquirePreferred("workbuddy", "hy3", nil, "", false); err == nil {
		t.Fatal("reserve guard must block")
	} else if se, ok := err.(*AcquireError); !ok || se.Rejections[0].Reason != "reserve_credits" {
		t.Fatalf("rejection: %v", err)
	}
}

func TestRealmPreference(t *testing.T) {
	p := New(nil)
	for _, r := range []struct{ id, realm, key string }{
		{"intl-1", "intl", "k-intl-1"},
		{"intl-2", "intl", "k-intl-2"},
		{"cn-1", "cn", "k-cn-1"},
	} {
		a := config.Account{ID: r.id, Provider: "workbuddy", APIKey: r.key, UID: "u-" + r.id, BaseURL: "https://www.workbuddy.ai", Realm: r.realm, Product: "desktop", MaxInFlight: 8}
		if err := p.Upsert(a, true, func(config.Account) error { return nil }); err != nil {
			t.Fatalf("upsert %s: %v", r.id, err)
		}
	}
	// With cn preference, cn-1 must be served first even though the cursor
	// starts at the intl accounts.
	got, err := p.AcquirePreferredRealm("workbuddy", "cn", "hy3", nil, "", false)
	if err != nil || got.ID != "cn-1" {
		t.Fatalf("realm preference: %v %v", got.ID, err)
	}
	p.Finish(got.ID, 200, Usage{}, false, 0, "hy3")
	// Preferred account id still wins over realm.
	got, err = p.AcquirePreferredRealm("workbuddy", "cn", "hy3", nil, "intl-2", false)
	if err != nil || got.ID != "intl-2" {
		t.Fatalf("preferred id must win: %v %v", got.ID, err)
	}
	p.Finish(got.ID, 200, Usage{}, false, 0, "hy3")
	// Fallback: intl preference still serves some account.
	got, err = p.AcquirePreferredRealm("workbuddy", "intl", "hy3", nil, "", false)
	if err != nil || got.ID == "" {
		t.Fatalf("fallback to other realm: %v %v", got.ID, err)
	}
	p.Finish(got.ID, 200, Usage{}, false, 0, "hy3")
}

func TestQuotaExhaustedParkAndLift(t *testing.T) {
	p := New(nil)
	a := config.Account{ID: "wb-1", Provider: "workbuddy", APIKey: "tok-1", UID: "u1", BaseURL: "https://www.workbuddy.ai", Realm: "intl", Product: "desktop", MaxInFlight: 8}
	_ = p.Upsert(a, true, func(config.Account) error { return nil })
	p.MarkQuotaExhausted("wb-1", time.Now().Add(time.Hour))
	if _, err := p.AcquirePreferred("workbuddy", "hy3", nil, "", false); err == nil {
		t.Fatal("parked account must not be schedulable")
	}
	// A positive balance observation lifts the park.
	now := time.Now()
	q := &provider.Quota{Source: "test", ObservedAt: now, ValidUntil: now.Add(5 * time.Minute), Buckets: []provider.Bucket{{Name: "p", Pool: "credits", Unit: "credits", Remaining: 10, Limit: 100}}}
	if err := p.SetQuota("wb-1", q); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AcquirePreferred("workbuddy", "hy3", nil, "", false); err != nil {
		t.Fatalf("park must lift with positive balance: %v", err)
	}
}

func TestDailyCountersPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stateFile := dir + "/state.json"
	p := New([]config.Account{{ID: "wb-1", Provider: "workbuddy", APIKey: "tok-1", UID: "u1", BaseURL: "https://www.workbuddy.ai", Realm: "intl", Product: "desktop", MaxInFlight: 8}})
	p.SetGuards(config.Guards{DailyTokens: 1000})
	if _, err := p.AcquirePreferred("workbuddy", "hy3", nil, "", false); err != nil {
		t.Fatal(err)
	}
	p.Finish("wb-1", 200, Usage{Total: 77, Known: true}, false, 0, "hy3")
	if err := p.Save(stateFile); err != nil {
		t.Fatal(err)
	}
	p2 := New([]config.Account{{ID: "wb-1", Provider: "workbuddy", APIKey: "tok-1", UID: "u1", BaseURL: "https://www.workbuddy.ai", Realm: "intl", Product: "desktop", MaxInFlight: 8}})
	if err := p2.Load(stateFile); err != nil {
		t.Fatal(err)
	}
	if _, err := p2.AcquirePreferred("workbuddy", "hy3", nil, "", false); err != nil {
		t.Fatalf("77/1000 must pass after restore: %v", err)
	}
	p2.mu.Lock()
	got := p2.entries[0].state.DailyTokens["hy3"]
	p2.mu.Unlock()
	if got != 77 {
		t.Fatalf("daily counter restored = %d", got)
	}
}
