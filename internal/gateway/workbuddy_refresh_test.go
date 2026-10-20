package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"context"
	"daydayup/internal/accounts"

	"daydayup/internal/config"
)

func tokenWithExp(access, refresh string, expUnix int64) (string, string) {
	payload, _ := json.Marshal(map[string]any{"exp": expUnix})
	return access + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig", refresh
}

func TestWorkBuddy401RefreshRetry(t *testing.T) {
	var chatCalls, refreshCalls atomic.Int64
	var lastAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"data":{"accessToken":"new-access-1","refreshToken":"new-refresh-1"}}}`)
			return
		}
		n := chatCalls.Add(1)
		lastAuth.Store(r.Header.Get("Authorization"))
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"code":401,"message":"token expired"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","usage":{"total_tokens":1}}`)
	}))
	defer srv.Close()
	exp := time.Now().Add(30 * time.Minute).Unix()
	access, refresh := tokenWithExp("old-access", "old-refresh", exp)
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, DBFile: filepath.Join(t.TempDir(), "gateway.db"), Accounts: []config.Account{{ID: "wb-1", Provider: "workbuddy", BaseURL: srv.URL, APIKey: access, RefreshToken: refresh, UID: "uid-1", Realm: "intl", Product: "desktop", MaxInFlight: 8}}}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	w := call(s, "POST", "/v1/chat/completions", `{"model":"hy3","messages":[{"role":"user","content":"x"}]}`)
	if w.Code != 200 {
		t.Fatalf("401 must refresh and retry: %d %s", w.Code, w.Body)
	}
	if chatCalls.Load() != 2 {
		t.Fatalf("chat calls = %d", chatCalls.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d", refreshCalls.Load())
	}
	if auth, _ := lastAuth.Load().(string); auth != "Bearer new-access-1" {
		t.Fatalf("retry auth = %q", auth)
	}
	// The refreshed token must be live in the pool.
	acc, err := s.Pool.Account("wb-1")
	if err != nil || acc.APIKey != "new-access-1" || acc.RefreshToken != "new-refresh-1" {
		t.Fatalf("pool tokens not swapped: %+v err=%v", acc, err)
	}
}

func TestWorkBuddy401RefreshOnceThenFailover(t *testing.T) {
	var chatCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"data":{"accessToken":"new","refreshToken":"new-r"}}}`)
			return
		}
		chatCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"code":401}}`)
	}))
	defer srv.Close()
	exp := time.Now().Add(30 * time.Minute).Unix()
	access, refresh := tokenWithExp("old-access", "old-refresh", exp)
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, DBFile: filepath.Join(t.TempDir(), "gateway.db"), Accounts: []config.Account{
		{ID: "wb-1", Provider: "workbuddy", BaseURL: srv.URL, APIKey: access, RefreshToken: refresh, UID: "uid-1", Realm: "intl", Product: "desktop", MaxInFlight: 8},
		{ID: "wb-2", Provider: "workbuddy", BaseURL: srv.URL, APIKey: access, RefreshToken: refresh, UID: "uid-2", Realm: "intl", Product: "desktop", MaxInFlight: 8},
	}}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	w := call(s, "POST", "/v1/chat/completions", `{"model":"hy3","messages":[{"role":"user","content":"x"}]}`)
	if w.Code != 401 {
		t.Fatalf("persistent 401 must surface: %d", w.Code)
	}
	// One refresh total across the request (attempt 1 refreshes, the retry and
	// subsequent failover never refresh again), then bounded by MaxAttempts.
	if chatCalls.Load() != 3 {
		t.Fatalf("chat calls = %d", chatCalls.Load())
	}
}

func TestRefreshLoopRotatesNearExpiry(t *testing.T) {
	var refreshCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"data":{"accessToken":"rotated-access","refreshToken":"rotated-refresh"}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "accounts.enc")
	exp := time.Now().Add(30 * time.Minute).Unix() // inside the 1h window
	access, refresh := tokenWithExp("near-expiry", "ref-token", exp)
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, DBFile: store, Accounts: []config.Account{{ID: "wb-1", Provider: "workbuddy", BaseURL: srv.URL, APIKey: access, RefreshToken: refresh, UID: "uid-1", Realm: "intl", Product: "desktop", MaxInFlight: 8}}}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s.refreshWorkBuddyLoop(context.Background(), 1*time.Hour)
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh loop must rotate near-expiry tokens, calls=%d", refreshCalls.Load())
	}
	acc, _ := s.Pool.Account("wb-1")
	if acc.APIKey != "rotated-access" || acc.RefreshToken != "rotated-refresh" {
		t.Fatalf("pool tokens: %+v", acc)
	}
	// The encrypted store must carry the rotated pair for the next boot.
	st, storeErr := accounts.New(store)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	loaded, err := st.Load(c.Accounts)
	if err != nil {
		t.Fatalf("store reload: %v", err)
	}
	if len(loaded) != 1 || loaded[0].APIKey != "rotated-access" || loaded[0].RefreshToken != "rotated-refresh" {
		t.Fatalf("store did not persist rotated tokens: %+v", loaded)
	}
}

func TestRefreshLoopSkipsFreshTokens(t *testing.T) {
	var refreshCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			refreshCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"accessToken":"x"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	exp := time.Now().Add(72 * time.Hour).Unix()
	access, refresh := tokenWithExp("fresh-access", "ref-token", exp)
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Accounts: []config.Account{{ID: "wb-1", Provider: "workbuddy", BaseURL: srv.URL, APIKey: access, RefreshToken: refresh, UID: "uid-1", Realm: "intl", Product: "desktop", MaxInFlight: 8}}}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s.refreshWorkBuddyLoop(context.Background(), 1*time.Hour)
	if refreshCalls.Load() != 0 {
		t.Fatalf("fresh tokens must not rotate, calls=%d", refreshCalls.Load())
	}
}

func TestRefreshConcurrencySerialized(t *testing.T) {
	var inFlight, peak, refreshCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			http.NotFound(w, r)
			return
		}
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		refreshCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"data":{"accessToken":"tok-%d","refreshToken":"ref-%d"}}`, refreshCalls.Load(), refreshCalls.Load()))
	}))
	defer srv.Close()
	exp := time.Now().Add(30 * time.Minute).Unix()
	access, refresh := tokenWithExp("old", "ref", exp)
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, DBFile: filepath.Join(t.TempDir(), "gateway.db"), Accounts: []config.Account{{ID: "wb-1", Provider: "workbuddy", BaseURL: srv.URL, APIKey: access, RefreshToken: refresh, UID: "uid-1", Realm: "intl", Product: "desktop", MaxInFlight: 8}}}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	acc, _ := s.Pool.Account("wb-1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = s.refreshAccountTokens(context.Background(), acc)
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("refresh must be serialized per account, peak in-flight=%d", peak.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("expected exactly one refresh, got %d", refreshCalls.Load())
	}
}
