package workbuddy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"daydayup/internal/config"
)

func testAccount(apiKey, refreshToken, uid, realm, product string) config.Account {
	base := "https://www.workbuddy.ai"
	if realm == "cn" {
		base = "https://copilot.tencent.com"
	}
	a := config.Account{ID: "wb-1", Provider: "workbuddy", APIKey: apiKey, RefreshToken: refreshToken, UID: uid, Realm: realm, Product: product, BaseURL: base, MaxInFlight: 4}
	_ = config.NormalizeAccount(&a)
	return a
}

func TestRewritePath(t *testing.T) {
	w := WorkBuddy{}
	if p, ok := w.RewritePath("/v1/chat/completions"); !ok || p != "/v2/chat/completions" {
		t.Fatalf("got %q ok=%v", p, ok)
	}
	if p, ok := w.RewritePath("/v1/responses"); ok || p != "/v1/responses" {
		t.Fatalf("responses should not be rewritten, got %q ok=%v", p, ok)
	}
}

func TestAuthorizeSessionHeaders(t *testing.T) {
	a := testAccount("tok-1", "", "uid-123", "intl", "desktop")
	req := httptest.NewRequest(http.MethodPost, "https://x/v2/chat/completions", nil)
	WorkBuddy{}.AuthorizeSession(req, a, "sess-1", "hy3")
	check := func(key, want string) {
		if got := req.Header.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	check("Authorization", "Bearer tok-1")
	check("X-User-Id", "uid-123")
	check("X-Agent-Type", "main")
	check("X-IDE-Type", "WorkBuddy")
	check("X-IDE-Version", "5.8.4")
	check("X-Domain", "www.workbuddy.ai")
	check("X-Private-Data", "true")
	check("X-Product", "SaaS")
	check("X-CodeBuddy-Request", "1")
	check("Accept-Language", "en-US")
	check("User-Agent", "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0")
	if req.Header.Get("X-Conversation-ID") == "" || req.Header.Get("X-Request-ID") == "" || req.Header.Get("X-Conversation-Message-ID") == "" {
		t.Fatal("conversation id family missing")
	}
	if req.Header.Get("X-Conversation-Request-ID") != req.Header.Get("X-Root-Request-ID") {
		t.Fatal("root request id must equal conversation request id")
	}
	// Same session derives the same conversation id.
	req2 := httptest.NewRequest(http.MethodPost, "https://x/v2/chat/completions", nil)
	WorkBuddy{}.AuthorizeSession(req2, a, "sess-1", "hy3")
	if req.Header.Get("X-Conversation-ID") != req2.Header.Get("X-Conversation-ID") {
		t.Fatal("conversation id must be stable per (account, session)")
	}
	// Different account with the same session key must not share it.
	req3 := httptest.NewRequest(http.MethodPost, "https://x/v2/chat/completions", nil)
	other := a
	other.UID = "uid-999"
	WorkBuddy{}.AuthorizeSession(req3, other, "sess-1", "hy3")
	if req.Header.Get("X-Conversation-ID") == req3.Header.Get("X-Conversation-ID") {
		t.Fatal("conversation id must not correlate across accounts")
	}
}

func TestAuthorizeSessionCN(t *testing.T) {
	a := testAccount("tok", "", "uid-1", "cn", "desktop")
	req := httptest.NewRequest(http.MethodPost, "https://x/v2/chat/completions", nil)
	WorkBuddy{}.AuthorizeSession(req, a, "", "")
	if req.Header.Get("X-Domain") != "copilot.tencent.com" {
		t.Fatalf("cn domain = %q", req.Header.Get("X-Domain"))
	}
	if req.Header.Get("X-IDE-Version") != "5.5.6" {
		t.Fatalf("cn ide version = %q", req.Header.Get("X-IDE-Version"))
	}
	if req.Header.Get("Accept-Language") != "zh-CN" {
		t.Fatalf("cn accept-language = %q", req.Header.Get("Accept-Language"))
	}
}

func TestAuthorizeNonChatKeepsIdentityOff(t *testing.T) {
	a := testAccount("tok", "", "uid-1", "intl", "desktop")
	req := httptest.NewRequest(http.MethodGet, "https://x/v1/completions", nil)
	WorkBuddy{}.AuthorizeSession(req, a, "s", "")
	if req.Header.Get("X-Agent-Type") != "" {
		t.Fatal("chat identity must not leak onto non-chat routes")
	}
	if req.Header.Get("X-User-Id") != "uid-1" {
		t.Fatal("credential headers still required")
	}
}

func TestModelsFallbackBundled(t *testing.T) {
	w := WorkBuddy{}
	a := testAccount("tok", "", "uid-1", "intl", "desktop")
	ctx := context.Background()
	// An unreachable upstream falls back to the bundled curated catalog.
	client := &http.Client{Timeout: 50 * time.Millisecond}
	models, err := w.Models(ctx, client, a)
	if err != nil {
		t.Fatalf("bundled fallback should not fail: %v", err)
	}
	if len(models) != 17 {
		t.Fatalf("intl bundled catalog: got %d models, want 17", len(models))
	}
	seen := map[string]bool{}
	for _, m := range models {
		id, _ := m["id"].(string)
		seen[id] = true
		if m["source"] != "catalog" {
			t.Fatalf("model %s missing source marker", id)
		}
	}
	for _, want := range []string{"hy4-preview-f", "deepseek-v4.1-flash", "gpt-6-astra", "kimi-k2.8-preview"} {
		if !seen[want] {
			t.Fatalf("bundled catalog missing %s", want)
		}
	}
	cn := testAccount("tok", "", "uid-1", "cn", "desktop")
	models, err = w.Models(ctx, client, cn)
	if err != nil || len(models) != 14 {
		t.Fatalf("cn bundled catalog: %d models, err=%v", len(models), err)
	}
}

func TestModelsRemoteCatalog(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"agents": map[string]any{
				"cli": map[string]any{
					"models": []string{"deepseek-v4.1-flash", "gpt-6-astra-x", "glm-5.3", "codewise-lite", "deepseek-v3-2-volc", "default-model", "fast-model", "deepseek-v4.1-flash-sg", "hy4-preview", "step-5-preview"},
				},
			},
			"models": []map[string]any{
				{"id": "deepseek-v4.1-flash", "maxInputTokens": 200000},
				{"id": "glm-5.3", "credits": "x0.79"},
				{"id": "gpt-6-astra-x", "credits": "x6.67"},
				{"id": "hy4-preview", "credits": "x0.29"},
				{"id": "hy4-preview-f", "credits": "x0.00"},
				{"id": "step-5-preview", "credits": "x1.0"},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/config" {
			t.Errorf("path %q", r.URL.Path)
		}
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("bad catalog headers: %v", r.Header)
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()
	a := testAccount("tok", "", "uid-1", "intl", "desktop")
	a.BaseURL = srv.URL
	models, err := WorkBuddy{}.Models(context.Background(), srv.Client(), a)
	if err != nil {
		t.Fatalf("remote catalog: %v", err)
	}
	// Curated: variants/aliases/paid siblings dropped, dedup merged with
	// bundled (glm-5.3 and deepseek-v4.1-flash already bundled); brand-new
	// live models (step-5-preview) are appended.
	if len(models) < 17 {
		t.Fatalf("merge should keep bundled + new, got %d", len(models))
	}
	dropped := map[string]bool{
		"gpt-6-astra-x": true, "codewise-lite": true, "deepseek-v3-2-volc": true,
		"default-model": true, "fast-model": true, "deepseek-v4.1-flash-sg": true,
		"hy4-preview": true, // paid sibling of hy4-preview-f
	}
	var sawStep bool
	for _, m := range models {
		id, _ := m["id"].(string)
		if dropped[id] {
			t.Fatalf("must be curated out, got %s", id)
		}
		if id == "step-5-preview" {
			sawStep = true
		}
	}
	if !sawStep {
		t.Fatal("new live model must be advertised")
	}
}

func TestQuotaParsesCreditPackages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/billing/meter/get-user-resource" {
			t.Errorf("path %q", r.URL.Path)
		}
		if r.Header.Get("X-Machine-ID") == "" || r.Header.Get("X-Session-ID") == "" {
			t.Error("billing fingerprint headers missing")
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"Response": map[string]any{
					"Data": map[string]any{
						"Accounts": []map[string]any{
							{"PackageName": "积分包A", "CycleCapacityRemainPrecise": 42.5, "CycleCapacitySizePrecise": 60},
							{"PackageName": "积分包B", "CapacityRemain": 3, "CapacitySize": 10},
						},
					},
				},
			},
		})
		_ = body
	}))
	defer srv.Close()
	a := testAccount("tok", "", "uid-1", "intl", "desktop")
	// Billing for intl realm hits the same origin; point it at the mock.
	a.BaseURL = srv.URL
	q, err := WorkBuddy{}.Quota(context.Background(), srv.Client(), a)
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if len(q.Buckets) != 2 {
		t.Fatalf("buckets = %d", len(q.Buckets))
	}
	if q.Buckets[0].Remaining != 42.5 || q.Buckets[0].Limit != 60 || q.Buckets[0].Unit != "credits" {
		t.Fatalf("bucket0 = %+v", q.Buckets[0])
	}
	if q.Buckets[1].Remaining != 3 {
		t.Fatalf("bucket1 fallback field failed: %+v", q.Buckets[1])
	}
	if q.RemainingTokens != nil {
		t.Fatal("credits must not masquerade as tokens")
	}
}

func TestErrorClassifiers(t *testing.T) {
	w := WorkBuddy{}
	if !w.ModelRateLimit([]byte(`{"error":{"code":6004}}`)) {
		t.Fatal("6004 must be a model rate limit")
	}
	if w.ModelRateLimit([]byte(`{"error":{"code":429}}`)) {
		t.Fatal("generic 429 must not be model-scoped")
	}
	if !w.Restriction([]byte(`{"error":{"code":11140,"message":"账号存在异常行为"}}`)) {
		t.Fatal("11140 must be a restriction")
	}
	if w.Restriction([]byte(`{}`)) {
		t.Fatal("empty body must not be a restriction")
	}
	if !w.ModelUnavailable([]byte(`{"error":{"code":11140}}`)) {
		t.Fatal("11140 must deny the model")
	}
	if !w.QuotaExhausted([]byte(`{}`)) {
		t.Fatal("402 body implies credits exhausted")
	}
}

func TestGzipTransformCLI(t *testing.T) {
	w := WorkBuddy{}
	a := testAccount("tok", "", "uid-1", "intl", "cli")
	body := []byte(`{"model":"hy3","metadata":{"x":1},"previous_response_id":"r-1","messages":[{"role":"user","content":"hi"}]}`)
	out, err := w.TransformRequestBody(http.MethodPost, "/v1/chat/completions", "application/json", body, a)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if bytes.Equal(out, body) {
		t.Fatal("cli body must be gzipped")
	}
	zr, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	plain, _ := io.ReadAll(zr)
	var parsed map[string]any
	if err := json.Unmarshal(plain, &parsed); err != nil {
		t.Fatalf("gzip body not valid JSON: %v", err)
	}
	// Official CLI behavior: cache_retention=long injected, Responses-API
	// state fields and client metadata dropped.
	if parsed["cache_retention"] != "long" {
		t.Fatalf("cache_retention missing: %v", parsed)
	}
	if _, ok := parsed["metadata"]; ok {
		t.Fatalf("metadata must be dropped: %v", parsed)
	}
	if _, ok := parsed["previous_response_id"]; ok {
		t.Fatalf("previous_response_id must be dropped: %v", parsed)
	}
	if parsed["model"] != "hy3" {
		t.Fatalf("model lost: %v", parsed)
	}
	h := w.BodyEncodingHeaders(a)
	if h.Get("Content-Encoding") != "gzip" {
		t.Fatal("cli must declare gzip encoding")
	}
	// Desktop passes through.
	ad := testAccount("tok", "", "uid-1", "intl", "desktop")
	out2, _ := w.TransformRequestBody(http.MethodPost, "/v1/chat/completions", "application/json", body, ad)
	if !bytes.Equal(out2, body) {
		t.Fatal("desktop body must pass through")
	}
	if w.BodyEncodingHeaders(ad) != nil {
		t.Fatal("desktop must not declare gzip")
	}
}

func TestRefreshRotatesTokens(t *testing.T) {
	var mu sync.Mutex
	var sawRefreshToken, sawSource string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			t.Errorf("path %q", r.URL.Path)
		}
		mu.Lock()
		sawRefreshToken = r.Header.Get("X-Refresh-Token")
		sawSource = r.Header.Get("X-Auth-Refresh-Source")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"data": map[string]any{
					"accessToken":  "new-access",
					"refreshToken": "new-refresh",
				},
			},
		})
	}))
	defer srv.Close()
	a := testAccount("old-access", "old-refresh", "uid-1", "intl", "desktop")
	a.BaseURL = srv.URL
	got, err := (WorkBuddy{}).Refresh(context.Background(), srv.Client(), a)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got.APIKey != "new-access" || got.RefreshToken != "new-refresh" {
		t.Fatalf("tokens = %q / %q", got.APIKey, got.RefreshToken)
	}
	if sawRefreshToken != "old-refresh" {
		t.Fatalf("sent refresh token %q", sawRefreshToken)
	}
	if sawSource != "plugin" {
		t.Fatalf("intl source %q", sawSource)
	}
	// CN realm sends the workbuddy source header.
	a2 := testAccount("old-access", "old-refresh", "uid-1", "cn", "desktop")
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawSource = r.Header.Get("X-Auth-Refresh-Source")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"accessToken": "x"}})
	}))
	defer srv2.Close()
	a2.BaseURL = srv2.URL
	if _, err := (WorkBuddy{}).Refresh(context.Background(), srv2.Client(), a2); err != nil {
		t.Fatalf("cn refresh: %v", err)
	}
	if sawSource != "workbuddy" {
		t.Fatalf("cn source %q", sawSource)
	}
}

func TestJWTExpiry(t *testing.T) {
	token := jwtFor(t, time.Now().Add(2*time.Hour).Unix())
	exp := jwtExp(token)
	if exp.Before(time.Now().Add(90 * time.Minute)) {
		t.Fatalf("exp too early: %v", exp)
	}
	if !jwtExp("not-a-jwt").IsZero() {
		t.Fatal("malformed token must yield zero time")
	}
}

func jwtFor(t *testing.T, exp int64) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"exp": exp})
	return "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
}

func TestConversationIDShape(t *testing.T) {
	id := conversationIDFor("uid-1", "sess-abc")
	if len(id) != 36 || !strings.Contains(id, "-") {
		t.Fatalf("conversation id shape: %q", id)
	}
	if conversationIDFor("uid-1", "  ") != "" {
		t.Fatal("empty session key must yield empty conversation id")
	}
}

func TestDeriveIDsStable(t *testing.T) {
	if deriveID("uid-1", "machine") != deriveID("uid-1", "machine") {
		t.Fatal("derive must be idempotent")
	}
	if deriveID("uid-1", "machine") == deriveID("uid-2", "machine") {
		t.Fatal("different accounts must differ")
	}
	for _, id := range []string{machineIDFor("u1"), sessionIDFor("u1"), newConversationID()} {
		if len(id) != 36 {
			t.Fatalf("uuid shape %q", id)
		}
	}
	if len(newMessageID()) != 32 {
		t.Fatal("message id must be 32 hex")
	}
}
func TestAuthorizeSessionVSCodeModelHeaders(t *testing.T) {
	a := testAccount("tok", "", "uid-1", "intl", "vscode")
	req := httptest.NewRequest(http.MethodPost, "https://x/v2/chat/completions", nil)
	WorkBuddy{}.AuthorizeSession(req, a, "sess-1", "gpt-6-astra")
	check := func(key, want string) {
		if got := req.Header.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	check("X-IDE-Type", "VSCode")
	check("X-IDE-Version", "1.119.0")
	check("X-Product-Version", "4.12.39997873")
	check("X-Env-ID", "production")
	check("X-Model-ID", "gpt-6-astra")
	check("X-Private-Data", "true") // intl only
	prepare := req.Header.Get("monitor_promptPrepareStartTime")
	send := req.Header.Get("monitor_httpSendTime")
	if prepare == "" || prepare != send {
		t.Fatalf("monitor timestamps must be equal and present: %q / %q", prepare, send)
	}
	if _, err := strconv.ParseInt(prepare, 10, 64); err != nil {
		t.Fatalf("monitor timestamp not millis: %q", prepare)
	}
	if req.Header.Get("X-B3-ParentSpanId") == "" || req.Header.Get("X-Device-Token-Error") == "" {
		t.Fatal("vscode b3/parent span and device-token-error missing")
	}
	// Without a model the extension sends no X-Model-ID / monitor stamps.
	req2 := httptest.NewRequest(http.MethodPost, "https://x/v2/chat/completions", nil)
	WorkBuddy{}.AuthorizeSession(req2, a, "sess-1", "")
	if req2.Header.Get("X-Model-ID") != "" || req2.Header.Get("monitor_promptPrepareStartTime") != "" {
		t.Fatal("model headers must be absent without a model")
	}
	// CN vscode identity sends neither X-Private-Data; the official extension
	// chats through www.workbuddy.cn, so the account pins that origin and the
	// X-Domain header must follow it.
	cn := testAccount("tok", "", "uid-1", "cn", "vscode")
	cn.BaseURL = "https://www.workbuddy.cn"
	req3 := httptest.NewRequest(http.MethodPost, "https://x/v2/chat/completions", nil)
	WorkBuddy{}.AuthorizeSession(req3, cn, "sess-1", "hy3")
	if req3.Header.Get("X-Private-Data") != "" {
		t.Fatal("cn vscode must not send X-Private-Data")
	}
	if req3.Header.Get("X-Domain") != "www.workbuddy.cn" {
		t.Fatalf("cn vscode domain %q", req3.Header.Get("X-Domain"))
	}
	// The default CN origin still advertises the realm desktop domain.
	cnDefault := testAccount("tok", "", "uid-1", "cn", "vscode")
	req4 := httptest.NewRequest(http.MethodPost, "https://x/v2/chat/completions", nil)
	WorkBuddy{}.AuthorizeSession(req4, cnDefault, "sess-1", "hy3")
	if req4.Header.Get("X-Domain") != "copilot.tencent.com" {
		t.Fatalf("cn default domain %q", req4.Header.Get("X-Domain"))
	}
}
