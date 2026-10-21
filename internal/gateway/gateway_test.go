package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/provider"
)

func newTestGateway(t *testing.T, upstream http.Handler, n int) (*Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	c := config.Config{DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, DBFile: filepath.Join(t.TempDir(), "gateway.db")}
	for i := 0; i < n; i++ {
		c.Accounts = append(c.Accounts, config.Account{ID: fmt.Sprintf("a%d", i), Provider: "sensenova", BaseURL: up.URL, APIKey: fmt.Sprintf("upstream-secret-%d", i), MaxInFlight: 8})
	}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return s, up
}
func call(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestRoundRobinTransparentRoutes(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-API-Key") != "" || r.Header.Get("X-Provider") != "" || r.Header.Get("X-Hop") != "" {
			t.Error("gateway credentials or hop headers leaked")
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != "{\"model\":\"m\",\"extra\":true}" {
			t.Errorf("body changed: %s", b)
		}
		if r.URL.RawQuery != "x=1&x=2&v=a%2Bb" {
			t.Errorf("query changed: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "sensitive=value")
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5},"custom":42}`)
	}), 2)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/images/generations", "/v1/future/opaque"} {
		r := httptest.NewRequest("POST", path+"?x=1&x=2&v=a%2Bb", strings.NewReader(`{"model":"m","extra":true}`))
		r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
		r.Header.Set("X-API-Key", s.Config.GatewayKey)
		r.Header.Set("X-Provider", "sensenova")
		r.Header.Set("Cookie", "private=1")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Connection", "X-Hop")
		r.Header.Set("X-Hop", "secret")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("X-Upstream") != "yes" || w.Header().Get("Set-Cookie") != "" || !strings.Contains(w.Body.String(), `"custom":42`) {
			t.Fatalf("unexpected response %d %s", w.Code, w.Body)
		}
	}
	for i, k := range keys {
		if k != fmt.Sprintf("Bearer upstream-secret-%d", i%2) {
			t.Errorf("rotation: %v", keys)
		}
	}
	states := s.Pool.Snapshot()
	if states[0].Stats.Total != 15 || states[1].Stats.Total != 10 || states[0].InFlight != 0 {
		t.Fatalf("usage/reservation: %+v", states)
	}
}

func TestRetryPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, method, path            string
		status, wantCalls, wantStatus int
	}{
		{"rate-limit", "POST", "/v1/chat/completions", 429, 2, 200},
		{"invalid-key", "POST", "/v1/messages", 401, 2, 200},
		{"ambiguous-generation", "POST", "/v1/responses", 503, 1, 503},
		{"policy-denied", "POST", "/v1/chat/completions", 403, 1, 403},
		{"unknown-write", "POST", "/v1/files", 429, 1, 429},
		{"safe-read", "GET", "/v1/files", 503, 2, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var count atomic.Int32
			s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				if r.Header.Get("Authorization") == "Bearer upstream-secret-0" {
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, `{"error":{"message":"rejected"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"usage":{"total_tokens":7}}`)
			}), 2)
			w := call(s, tt.method, tt.path, `{"model":"m"}`)
			if w.Code != tt.wantStatus || int(count.Load()) != tt.wantCalls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, count.Load(), w.Body)
			}
			states := s.Pool.Snapshot()
			for _, state := range states {
				if state.InFlight != 0 {
					t.Fatal("reservation leaked")
				}
			}
			if tt.status == 429 && states[0].CooldownUntil.Before(time.Now().Add(110*time.Second)) {
				t.Fatal("Retry-After not honored")
			}
		})
	}
}

func TestModelsDiscoveryAggregationAndAccountRouting(t *testing.T) {
	var chosen string
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			if r.Header.Get("Authorization") == "Bearer upstream-secret-0" {
				_, _ = io.WriteString(w, `{"data":[{"id":"a","context_length":123},{"id":"shared"}]}`)
			} else {
				_, _ = io.WriteString(w, `{"data":[{"id":"b"},{"id":"shared"}]}`)
			}
			return
		}
		chosen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	}), 2)
	w := call(s, "GET", "/v1/models", "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var response struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if len(response.Data) != 3 || response.Data[0]["context_length"] != float64(123) || response.Data[0]["owned_by"] != "sensenova" {
		t.Fatalf("models: %s", w.Body)
	}
	w = call(s, "POST", "/v1/chat/completions", `{"model":"b"}`)
	if w.Code != 200 || chosen != "Bearer upstream-secret-1" {
		t.Fatalf("wrong model account: %d %s", w.Code, chosen)
	}
	w = call(s, "POST", "/v1/chat/completions", `{"model":"missing"}`)
	if w.Code != 503 {
		t.Fatalf("unsupported model status: %d", w.Code)
	}
}

func TestMultipartAndEscapedPath(t *testing.T) {
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	_ = m.WriteField("model", "image-only")
	f, _ := m.CreateFormFile("image", "x.png")
	_, _ = f.Write([]byte{0, 255, 13, 10, 17})
	_ = m.Close()
	want := append([]byte(nil), body.Bytes()...)
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !bytes.Equal(b, want) {
			t.Error("multipart bytes changed")
		}
		if r.URL.EscapedPath() != "/v1/files/a%2Fb" {
			t.Errorf("raw path changed: %s", r.URL.EscapedPath())
		}
		w.WriteHeader(201)
	}), 2)
	s.Config.Accounts[0].Models = []string{"other"} // rebuild pool with model allowlists
	s.Config.Accounts[1].Models = []string{"image-only"}
	s = New(s.Config)
	r := httptest.NewRequest("POST", "/providers/sensenova/v1/files/a%2Fb", bytes.NewReader(want))
	r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	r.Header.Set("Content-Type", m.FormDataContentType())
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 201 || s.Pool.Snapshot()[1].Stats.Requests != 1 {
		t.Fatalf("multipart route: %d %+v", w.Code, s.Pool.Snapshot())
	}
}

func TestSSEFlushAndUsage(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	first := "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n"
	last := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\ndata: [DONE]\n\n"
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, last)
	}), 1)
	gw := httptest.NewServer(s)
	defer gw.Close()
	r, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	r.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, len(first))
	if _, err = io.ReadFull(resp.Body, b); err != nil || string(b) != first {
		t.Fatalf("first chunk was not flushed: %q %v", b, err)
	}
	unblock()
	tail, err := io.ReadAll(resp.Body)
	if err != nil || string(tail) != last {
		t.Fatalf("SSE changed: %q %v", tail, err)
	}
	deadline := time.Now().Add(time.Second)
	for s.Pool.Snapshot()[0].InFlight != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	state := s.Pool.Snapshot()[0]
	if state.Stats.Total != 14 || state.Stats.Failures != 0 {
		t.Fatalf("SSE accounting: %+v", state)
	}
}

func TestSecurityLimitsAndAdmin(t *testing.T) {
	var count atomic.Int32
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1); w.WriteHeader(200) }), 1)
	for _, tt := range []struct {
		path, key string
		want      int
	}{
		{"/admin/v1/accounts", "", 401}, {"/admin/v1/accounts", "gateway-secret-at-least-16", 401},
		{"/admin/v1/accounts", "admin-secret-at-least-16", 200}, {"/metrics", "gateway-secret-at-least-16", 401},
		{"/v1/chat/completions", "admin-secret-at-least-16", 401},
	} {
		r := httptest.NewRequest("GET", tt.path, nil)
		r.Header.Set("Authorization", "Bearer "+tt.key)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tt.want {
			t.Fatalf("%s: %d", tt.path, w.Code)
		}
		if strings.Contains(w.Body.String(), "upstream-secret") {
			t.Fatal("credential leaked")
		}
	}
	if call(s, "POST", "/v1/../admin", "").Code != 400 {
		t.Fatal("traversal accepted")
	}
	s.Config.MaxRequestBytes = 8
	if call(s, "POST", "/v1/chat/completions", strings.Repeat("a", 9)).Code != 413 {
		t.Fatal("body limit not enforced")
	}
	if count.Load() != 0 {
		t.Fatal("rejected requests reached upstream")
	}
	r := httptest.NewRequest("PUT", "/admin/v1/accounts/a0/enabled", strings.NewReader(`{"enabled":false}`))
	r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || s.Pool.Snapshot()[0].Enabled {
		t.Fatal("admin disable failed")
	}
}

func TestQuotaFeedUsesSeparateCredentials(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer quota-only-secret" {
			t.Error("wrong feed credential")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"source": "test-feed", "observed_at": time.Now().UTC(), "valid_until": time.Now().Add(time.Minute).UTC(), "remaining_tokens": 99})
	}))
	defer feed.Close()
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}), 1)
	s.Config.Accounts[0].QuotaFeedURL = feed.URL
	s.Config.Accounts[0].QuotaFeedKey = "quota-only-secret"
	s = New(s.Config)
	s.Refresh(context.Background())
	state := s.Pool.Snapshot()[0]
	if state.QuotaStatus != "reported" || state.Quota == nil || *state.Quota.RemainingTokens != 99 || state.QuotaSyncError != "" {
		t.Fatalf("quota feed: %+v", state)
	}
}

func TestRetryAfterDate(t *testing.T) {
	if retryDelay("9", time.Second) != 9*time.Second {
		t.Fatal("delta")
	}
	d := retryDelay(time.Now().Add(60*time.Second).UTC().Format(http.TimeFormat), time.Second)
	if d < 58*time.Second || d > 61*time.Second {
		t.Fatalf("HTTP-date: %s", d)
	}
	if retryDelay("bad", 7*time.Second) != 7*time.Second {
		t.Fatal("fallback")
	}
}

func TestTruncatedSSEIsNotReplayed(t *testing.T) {
	var count atomic.Int32
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
	}), 2)
	gw := httptest.NewServer(s)
	defer gw.Close()
	r, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || count.Load() != 1 {
		t.Fatalf("truncated stream looked successful or replayed: err=%v count=%d", err, count.Load())
	}
}

func TestUpstreamRedirectDoesNotReceiveCredentials(t *testing.T) {
	var redirected atomic.Bool
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Store(true) }))
	defer dest.Close()
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
	}), 1)
	w := call(s, "GET", "/v1/redirect", "")
	if w.Code != http.StatusTemporaryRedirect || redirected.Load() {
		t.Fatal("followed credential-bearing redirect")
	}
}

func TestEngineRateLimitOnlyCoolsSelectedModel(t *testing.T) {
	const errorBody = `{"error":{"message":"inference exceeds tpm/rpm limit","type":"rate_limit_error","code":"insufficient_quota"}}`
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"limited"},{"id":"available"}]}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "limited") {
			w.Header().Set("Retry-After", "120")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, errorBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":1}}`)
	}), 1)
	limitedCall := func() *httptest.ResponseRecorder {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		return contextCall(s, ctx, "POST", "/v1/chat/completions", `{"model":"limited"}`, nil)
	}
	w := limitedCall()
	if w.Code != 504 || !strings.Contains(w.Body.String(), "pool_wait_timeout") {
		t.Fatalf("limited request did not wait until deadline: %d %s", w.Code, w.Body)
	}
	if limitedCall().Code != 504 {
		t.Fatal("limited model did not stay in cooldown")
	}
	if call(s, "POST", "/v1/chat/completions", `{"model":"available"}`).Code != 200 {
		t.Fatal("unrelated model was cooled")
	}
	s.Refresh(context.Background())
	if limitedCall().Code != 504 {
		t.Fatal("discovery reset Retry-After cooldown")
	}
	state := s.Pool.Snapshot()[0]
	if !state.CooldownUntil.IsZero() || time.Until(state.ModelCooldowns["limited"]) < 110*time.Second {
		t.Fatalf("wrong scope: %+v", state)
	}
}

func TestCatalogExcludesObservedUnavailableModels(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"denied"},{"id":"available"}]}`)
			return
		}
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"error":{"message":"model is not available in the current token plan","type":"permission_denied_error","code":"7"}}`)
	}), 1)
	s.Refresh(context.Background())
	if call(s, "POST", "/v1/chat/completions", `{"model":"denied"}`).Code != 403 {
		t.Fatal("permission error changed")
	}
	s.Refresh(context.Background())
	w := call(s, "GET", "/v1/models", "")
	var models struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &models)
	if len(models.Data) != 1 || models.Data[0]["id"] != "available" {
		t.Fatalf("unavailable model advertised: %s", w.Body)
	}
	if _, ok := s.Pool.Snapshot()[0].UnavailableModels["denied"]; !ok {
		t.Fatal("restriction is not visible to admin")
	}
}

// stubProvider is a hermetic platform adapter: discovery reports the models
// declared on the stub, so cross-platform and cross-group routing is fully
// under test control (no real adapter catalog fallbacks).
type stubProvider struct {
	name   string
	models []provider.Model
}

func (p stubProvider) Name() string                            { return p.name }
func (p stubProvider) Authorize(*http.Request, config.Account) {}
func (p stubProvider) Models(context.Context, *http.Client, config.Account) ([]provider.Model, error) {
	return p.models, nil
}
func (stubProvider) Quota(context.Context, *http.Client, config.Account) (provider.Quota, error) {
	return provider.Quota{}, provider.ErrQuotaUnsupported
}
func (stubProvider) ModelRateLimit([]byte) bool   { return false }
func (stubProvider) ModelUnavailable([]byte) bool { return false }

func TestCrossPlatformFailoverAndGlobalCatalog(t *testing.T) {
	var calls atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":7}}`)
	}))
	defer up2.Close()
	reg := provider.NewRegistry()
	_ = reg.Register("p1", stubProvider{name: "p1", models: []provider.Model{{"id": "m"}}})
	_ = reg.Register("p2", stubProvider{name: "p2", models: []provider.Model{{"id": "m"}}})
	c := config.Config{GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Accounts: []config.Account{
		{ID: "p1-a", Provider: "p1", BaseURL: up1.URL, APIKey: "{{SECRET_xxx}}", MaxInFlight: 8},
		{ID: "p2-a", Provider: "p2", BaseURL: up2.URL, APIKey: "...}}", MaxInFlight: 8},
	}}
	s := NewWithProviders(c, reg)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// No X-Provider: the request must fail over from platform p1 to p2.
	w := call(s, "POST", "/v1/chat/completions", `{"model":"m"}`)
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatalf("cross-platform failover failed: %d calls=%d %s", w.Code, calls.Load(), w.Body)
	}
	// The aggregated catalog advertises one model served by both platforms.
	w = call(s, "GET", "/v1/models", "")
	var models struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &models)
	if w.Code != 200 || len(models.Data) != 1 || models.Data[0]["id"] != "m" || models.Data[0]["owned_by"] != "p1" {
		t.Fatalf("global catalog: %d %s", w.Code, w.Body)
	}
	ps, _ := models.Data[0]["platforms"].([]any)
	if len(ps) != 2 || ps[0] != "p1" || ps[1] != "p2" {
		t.Fatalf("platforms not merged: %s", w.Body)
	}
}

func TestGroupRoutingHeaderAndCatalog(t *testing.T) {
	var hits atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Group") != "" {
			t.Error("X-Group leaked upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":7}}`)
	}))
	defer up2.Close()
	up3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":7}}`)
	}))
	defer up3.Close()
	reg := provider.NewRegistry()
	_ = reg.Register("p1", stubProvider{name: "p1", models: []provider.Model{{"id": "m"}}})
	_ = reg.Register("p2", stubProvider{name: "p2", models: []provider.Model{{"id": "m"}}})
	_ = reg.Register("p3", stubProvider{name: "p3", models: []provider.Model{{"id": "m2"}}})
	c := config.Config{GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Accounts: []config.Account{
		{ID: "g1-p1", Provider: "p1", Group: "shared", BaseURL: up1.URL, APIKey: "{{SECRET_...}}", MaxInFlight: 8},
		{ID: "g1-p2", Provider: "p2", Group: "shared", BaseURL: up2.URL, APIKey: "...", MaxInFlight: 8},
		{ID: "g2-p3", Provider: "p3", Group: "isolated", BaseURL: up3.URL, APIKey: "...", MaxInFlight: 8},
	}}
	s := NewWithProviders(c, reg)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// Run discovery once so each account's real model list gates eligibility
	// (before discovery, accounts optimistically support any model).
	s.Refresh(context.Background())
	groupCall := func(group, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Group", group)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	// Group "shared": cross-platform failover stays inside the group.
	w := groupCall("shared", "POST", "/v1/chat/completions", `{"model":"m"}`)
	if w.Code != 200 {
		t.Fatalf("group failover failed: %d %s", w.Code, w.Body)
	}
	// Group "isolated" cannot reach shared's accounts for model m.
	w = groupCall("isolated", "POST", "/v1/chat/completions", `{"model":"m"}`)
	if w.Code != 503 {
		t.Fatalf("cross-group leak: %d %s", w.Code, w.Body)
	}
	// ... but serves its own model.
	w = groupCall("isolated", "POST", "/v1/chat/completions", `{"model":"m2"}`)
	if w.Code != 200 {
		t.Fatalf("isolated own model: %d %s", w.Code, w.Body)
	}
	// Catalog scoped per group.
	w = groupCall("shared", "GET", "/v1/models", "")
	var models struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &models)
	if w.Code != 200 || len(models.Data) != 1 || models.Data[0]["id"] != "m" {
		t.Fatalf("shared catalog: %d %s", w.Code, w.Body)
	}
	ps, _ := models.Data[0]["platforms"].([]any)
	if len(ps) != 2 || ps[0] != "p1" || ps[1] != "p2" {
		t.Fatalf("shared platforms: %s", w.Body)
	}
	w = groupCall("isolated", "GET", "/v1/models", "")
	_ = json.Unmarshal(w.Body.Bytes(), &models)
	if w.Code != 200 || len(models.Data) != 1 || models.Data[0]["id"] != "m2" {
		t.Fatalf("isolated catalog: %d %s", w.Code, w.Body)
	}
	// No header: the default group has no accounts, so nothing is advertised.
	w = call(s, "GET", "/v1/models", "")
	if w.Code != 503 {
		t.Fatalf("default group should be empty: %d %s", w.Code, w.Body)
	}
	// Invalid group names are rejected.
	w = groupCall("bad group!", "GET", "/v1/models", "")
	if w.Code != 400 {
		t.Fatalf("invalid group accepted: %d", w.Code)
	}
}

func TestGroupRoutingPathPrefix(t *testing.T) {
	var hits atomic.Int32
	upShared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Group") != "" {
			t.Error("X-Group leaked upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":7}}`)
	}))
	defer upShared.Close()
	upIsolated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	}))
	defer upIsolated.Close()
	reg := provider.NewRegistry()
	_ = reg.Register("p1", stubProvider{name: "p1", models: []provider.Model{{"id": "m"}}})
	_ = reg.Register("p2", stubProvider{name: "p2", models: []provider.Model{{"id": "m2"}}})
	c := config.Config{GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Accounts: []config.Account{
		{ID: "g1-p1", Provider: "p1", Group: "shared", BaseURL: upShared.URL, APIKey: "sk-shared-01", MaxInFlight: 8},
		{ID: "g2-p2", Provider: "p2", Group: "isolated", BaseURL: upIsolated.URL, APIKey: "sk-isolated-01", MaxInFlight: 8},
	}}
	s := NewWithProviders(c, reg)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s.Refresh(context.Background())
	pathCall := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+c.GatewayKey)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	w := pathCall("POST", "/groups/shared/v1/chat/completions", `{"model":"m"}`)
	if w.Code != 200 {
		t.Fatalf("path group failover: %d %s", w.Code, w.Body)
	}
	w = pathCall("POST", "/groups/isolated/v1/chat/completions", `{"model":"m"}`)
	if w.Code != 503 {
		t.Fatalf("path cross-group leak: %d %s", w.Code, w.Body)
	}
	w = pathCall("GET", "/groups/shared/v1/models", "")
	var models struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &models)
	if w.Code != 200 || len(models.Data) != 1 || models.Data[0]["id"] != "m" {
		t.Fatalf("path shared catalog: %d %s", w.Code, w.Body)
	}
	// An empty path segment normalizes to the default group rather than being
	// rejected: the request reaches the catalog path (which has no fresh
	// discovery for the empty default group) instead of failing as invalid.
	w = pathCall("GET", "/groups//v1/models", "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "models_unavailable") {
		t.Fatalf("empty path group should resolve to the default group: %d %s", w.Code, w.Body)
	}
	w = pathCall("GET", "/groups/bad%20group!/v1/models", "")
	if w.Code != 400 {
		t.Fatalf("invalid path group accepted: %d", w.Code)
	}
	w = pathCall("GET", "/groups/shared", "")
	if w.Code != 404 {
		t.Fatalf("bare group path accepted: %d", w.Code)
	}
}
