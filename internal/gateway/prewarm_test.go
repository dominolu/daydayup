package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"safuno/internal/config"
)

func TestNextPrewarmTime(t *testing.T) {
	now := time.Date(2026, 10, 19, 3, 30, 0, 0, time.Local)
	next := nextPrewarmTime(now, 4)
	if next.Hour() != 4 || !next.After(now) {
		t.Fatalf("next = %v", next)
	}
	now2 := time.Date(2026, 10, 19, 5, 0, 0, 0, time.Local)
	next2 := nextPrewarmTime(now2, 4)
	if next2.Day() != 20 {
		t.Fatalf("next day rollover = %v", next2)
	}
}

func TestRunPrewarmPingsAccounts(t *testing.T) {
	var chatCalls atomic.Int64
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			chatCalls.Add(1)
			b, _ := io.ReadAll(r.Body)
			if len(b) == 0 {
				t.Error("empty prewarm body")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"p","model":"m","choices":[],"usage":{}}`))
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
	})
	gw := httptest.NewServer(up)
	t.Cleanup(gw.Close)
	c := config.Config{DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Prewarm: config.PrewarmConfig{Enabled: true}}
	c.Accounts = append(c.Accounts, config.Account{ID: "a1", Provider: "sensenova", BaseURL: gw.URL, APIKey: "k1", MaxInFlight: 8})
	s := New(c)
	// Discover the account's model through the normal probe path.
	s.Refresh(context.Background())
	if len(s.Pool.Snapshot()[0].Models) == 0 {
		t.Fatalf("discovery failed: %+v", s.Pool.Snapshot()[0])
	}
	s.runPrewarm(context.Background(), "manual")
	if chatCalls.Load() == 0 {
		t.Fatalf("prewarm did not ping any account")
	}
	if s.Pool.Snapshot()[0].Stats.Requests != 0 {
		t.Fatalf("prewarm mutated pool accounting: %+v", s.Pool.Snapshot()[0].Stats)
	}
	_ = time.Now
}

func TestPrewarmHourZeroSupported(t *testing.T) {
	cfg := config.PrewarmConfig{Enabled: true, Hour: ptr(0)}
	c := config.Config{Prewarm: cfg}
	s := &Server{Config: c}
	if s.prewarmHour() != 0 {
		t.Fatalf("hour 0 must be honored, got %d", s.prewarmHour())
	}
	cfg2 := config.PrewarmConfig{Enabled: true}
	c2 := config.Config{Prewarm: cfg2}
	s2 := &Server{Config: c2}
	if s2.prewarmHour() != 4 {
		t.Fatalf("default should be 4, got %d", s2.prewarmHour())
	}
	next := nextPrewarmTime(time.Date(2026, 10, 19, 0, 30, 0, 0, time.Local), 0)
	if next.Hour() != 0 || !next.After(time.Date(2026, 10, 19, 0, 30, 0, 0, time.Local)) {
		t.Fatalf("nextPrewarmTime at midnight wrong: %v", next)
	}
}

func ptr(v int) *int { return &v }
