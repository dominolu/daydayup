package gateway

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"daydayup/internal/accounts"
	"daydayup/internal/config"
)

// newWorkBuddyGateway builds a gateway whose default provider is workbuddy
// and whose accounts point at the given mock upstream.
func newWorkBuddyGateway(t *testing.T, upstream http.Handler, n int) (*Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3}
	for i := 0; i < n; i++ {
		c.Accounts = append(c.Accounts, config.Account{ID: fmt.Sprintf("wb%d", i), Provider: "workbuddy", BaseURL: up.URL, APIKey: fmt.Sprintf("access-%d", i), RefreshToken: fmt.Sprintf("refresh-%d", i), UID: fmt.Sprintf("uid-%d", i), Realm: "intl", Product: "desktop", MaxInFlight: 8})
	}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return s, up
}

func TestWorkBuddyChatPassthrough(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var convIDs, reqIDs []string
	var domains []string
	s, _ := newWorkBuddyGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		convIDs = append(convIDs, r.Header.Get("X-Conversation-ID"))
		reqIDs = append(reqIDs, r.Header.Get("X-Request-ID"))
		domains = append(domains, r.Header.Get("X-Domain"))
		mu.Unlock()
		if r.Header.Get("X-Agent-Type") != "main" || r.Header.Get("X-IDE-Type") != "WorkBuddy" {
			t.Errorf("identity headers missing: %v", r.Header)
		}
		if r.Header.Get("X-Conversation-Request-ID") != r.Header.Get("X-Root-Request-ID") {
			t.Errorf("root/request id mismatch")
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"model":"hy3","messages":[{"role":"user","content":"hi"}]}` {
			t.Errorf("body changed: %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
	}), 1)
	body := `{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Session-ID", "wb-session-1")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body)
		}
		if w.Header().Get("X-Session-ID") != "wb-session-1" {
			t.Fatalf("session response header %q", w.Header().Get("X-Session-ID"))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if p != "/v2/chat/completions" {
			t.Fatalf("upstream path %q, want /v2/chat/completions", p)
		}
	}
	// Same gateway session on the same account must reuse the same
	// conversation id but rotate request ids, exactly like the official
	// client.
	if len(convIDs) != 2 || convIDs[0] == "" || convIDs[0] != convIDs[1] {
		t.Fatalf("conversation ids: %v", convIDs)
	}
	if reqIDs[0] == reqIDs[1] {
		t.Fatalf("request ids must rotate: %v", reqIDs)
	}
	// X-Domain follows the account origin: the mock base URL host here.
	wantDomain := strings.TrimPrefix(s.Pool.Accounts()[0].BaseURL, "http://")
	if domains[0] != wantDomain {
		t.Fatalf("domain %q, want %q", domains[0], wantDomain)
	}
}

func TestWorkBuddy402Failover(t *testing.T) {
	var calls atomic.Int64
	s, _ := newWorkBuddyGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			// First account is out of credits.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(402)
			_, _ = io.WriteString(w, `{"error":{"code":402,"message":"insufficient credits"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","usage":{"total_tokens":1}}`)
	}), 2)
	w := call(s, "POST", "/v1/chat/completions", `{"model":"hy3","messages":[{"role":"user","content":"x"}]}`)
	if w.Code != 200 {
		t.Fatalf("402 must fail over: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", calls.Load())
	}
	// The exhausted account is parked until the next 04:00 local reset.
	states := s.Pool.Snapshot()
	var parked bool
	for _, st := range states {
		if st.QuotaExhaustedUntil != nil && !st.QuotaExhaustedUntil.IsZero() {
			parked = true
		}
	}
	if !parked {
		t.Fatal("exhausted account must be parked")
	}
}

func TestWorkBuddyRestrictionHoldsRealm(t *testing.T) {
	var calls atomic.Int64
	s, _ := newWorkBuddyGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"error":{"code":11140,"message":"账号存在异常行为，已被限制使用"}}`)
	}), 2)
	w := call(s, "POST", "/v1/chat/completions", `{"model":"hy3","messages":[{"role":"user","content":"x"}]}`)
	if w.Code != 403 {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	states := s.Pool.Snapshot()
	for _, st := range states {
		if st.CooldownUntil.IsZero() {
			t.Fatalf("restriction must cool the whole realm: %+v", st)
		}
	}
}

func TestWorkBuddyGzipCLI(t *testing.T) {
	var gzipped bool
	var body []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") == "gzip" {
			gzipped = true
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip: %v", err)
				return
			}
			b, _ := io.ReadAll(zr)
			body = b
		} else {
			b, _ := io.ReadAll(r.Body)
			body = b
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("cli accept %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("X-IDE-Type") != "CLI" || r.Header.Get("X-Agent-Runtime") != "other" || r.Header.Get("X-Private-Data") != "false" {
			t.Errorf("cli identity headers missing: %v", r.Header)
		}
		if r.Header.Get("x-stainless-lang") != "js" || r.Header.Get("x-stainless-package-version") != "6.25.0" {
			t.Errorf("stainless fingerprint missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","usage":{"total_tokens":1}}`)
	}))
	defer up.Close()
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Accounts: []config.Account{{ID: "cli-1", Provider: "workbuddy", BaseURL: up.URL, APIKey: "access-cli", UID: "uid-cli", Realm: "intl", Product: "cli", MaxInFlight: 8}}}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	w := call(s, "POST", "/v1/chat/completions", `{"model":"hy3","metadata":{"x":1},"messages":[{"role":"user","content":"x"}]}`)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if !gzipped {
		t.Fatal("cli request must be gzipped")
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("gzip body not JSON: %s", body)
	}
	if parsed["cache_retention"] != "long" {
		t.Fatalf("cache_retention not injected: %s", body)
	}
	if _, ok := parsed["metadata"]; ok {
		t.Fatalf("metadata must be dropped: %s", body)
	}
}

func TestWorkBuddyModelsViaGateway(t *testing.T) {
	s, _ := newWorkBuddyGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"hy3","name":"Hy3"}]}`)
	}), 1)
	w := call(s, "GET", "/v1/models", "")
	if w.Code != 200 {
		t.Fatalf("models: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "hy3") || !strings.Contains(w.Body.String(), "hy4-preview-f") {
		t.Fatalf("model list: %s", w.Body)
	}
}

func TestWorkBuddyTokenImport(t *testing.T) {
	dir := t.TempDir()
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, DBFile: dir + "/gateway.db"}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	body := `{"accounts":[{"id":"wb-intl","name":"国际版主号","uid":"u-1","access_token":"at-1","refresh_token":"rt-1","realm":"intl","product":"desktop","max_in_flight":4},{"id":"wb-cn","uid":"u-2","access_token":"at-2","refresh_token":"rt-2","realm":"cn","product":"cli"}]}`
	r := httptest.NewRequest("POST", "/admin/v1/accounts/import", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("import: %d %s", w.Code, w.Body)
	}
	var resp struct {
		Imported int `json:"imported"`
		Errors   []map[string]string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Imported != 2 || len(resp.Errors) != 0 {
		t.Fatalf("import result: %+v", resp)
	}
	acc, err := s.Pool.Account("wb-cn")
	if err != nil || acc.UID != "u-2" || acc.Product != "cli" || acc.Realm != "cn" {
		t.Fatalf("imported account: %+v err=%v", acc, err)
	}
	// The tokens must land in the encrypted store.
	st, storeErr := accounts.New(c.DBFile)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	loaded, err := st.Load(c.Accounts)
	if err != nil || len(loaded) != 2 {
		t.Fatalf("store after import: %v %v", loaded, err)
	}
	byID := map[string]config.Account{}
	for _, a := range loaded {
		byID[a.ID] = a
	}
	if byID["wb-intl"].APIKey != "at-1" || byID["wb-intl"].RefreshToken != "rt-1" || byID["wb-cn"].APIKey != "at-2" {
		t.Fatalf("store tokens: %+v", byID)
	}
}

func TestWorkBuddyVSCodeModelHeaders(t *testing.T) {
	var mu sync.Mutex
	var modelID, monitor string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		modelID = r.Header.Get("X-Model-ID")
		monitor = r.Header.Get("monitor_promptPrepareStartTime")
		mu.Unlock()
		if r.Header.Get("X-IDE-Type") != "VSCode" || r.Header.Get("X-Private-Data") != "true" {
			t.Errorf("vscode identity headers missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok","usage":{"total_tokens":1}}`)
	}))
	defer up.Close()
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Accounts: []config.Account{{ID: "vs-1", Provider: "workbuddy", BaseURL: up.URL, APIKey: "access-vs", UID: "uid-vs", Realm: "intl", Product: "vscode", MaxInFlight: 8}}}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	w := call(s, "POST", "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"x"}]}`)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if modelID != "gpt-6-astra" {
		t.Fatalf("X-Model-ID %q", modelID)
	}
	if monitor == "" {
		t.Fatal("monitor timestamp missing")
	}
}
