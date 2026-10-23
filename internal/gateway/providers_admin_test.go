package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"safuno/internal/config"
	"safuno/internal/provider"
)

// loadConfigDoc runs the production config loader on a document written to a
// temporary file (the loader needs the gateway keys in the environment).
func loadConfigDoc(t *testing.T, doc string) (config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		return config.Config{}, err
	}
	t.Setenv("GATEWAY_API_KEY", "gateway-secret-at-least-16")
	t.Setenv("GATEWAY_ADMIN_KEY", "admin-secret-at-least-16")
	return config.Load(path)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newProviderGateway builds a gateway whose config file exists (the admin API
// writes platform registrations back into it) and whose upstream is an
// OpenAI-compatible mock serving models and chat completions.
func newProviderGateway(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"hot-model"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"r1","model":"hot-model","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"total_tokens":3}}`)
	}))
	t.Cleanup(up.Close)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	raw := fmt.Sprintf(`{"listen":"127.0.0.1:0","default_provider":"sensenova","gateway_key_env":"GATEWAY_API_KEY","admin_key_env":"GATEWAY_ADMIN_KEY","accounts":[]}`)
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c := config.Config{
		GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16",
		RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3,
		DefaultProvider: "sensenova", DBFile: filepath.Join(dir, "gateway.db"), SourcePath: cfgPath,
	}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := s.LoadAccounts(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.accountStore.Close() })
	return s, up
}

func providerCatalog(t *testing.T, s *Server) []PlatformInfo {
	t.Helper()
	w := adminCall(s, "GET", "/admin/v1/providers", "")
	if w.Code != 200 {
		t.Fatalf("catalog: %d %s", w.Code, w.Body)
	}
	var body struct {
		Data []PlatformInfo `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}

func TestRegisterProviderHotAddsPlatform(t *testing.T) {
	s, up := newProviderGateway(t)
	body := fmt.Sprintf(`{"id":"nova","name":"Nova 实验平台","tagline":"OpenAI 兼容","base_url":%q,"id_prefix":"nova","hint":"本地模拟"}`, up.URL)
	w := adminCall(s, "POST", "/admin/v1/providers", body)
	if w.Code != 201 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	var added struct {
		Data PlatformInfo `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	// Every operator field survives the round trip through the registry.
	if added.Data.ID != "nova" || added.Data.Name != "Nova 实验平台" || added.Data.Tagline != "OpenAI 兼容" ||
		added.Data.IDPrefix != "nova" || added.Data.Hint != "本地模拟" || !added.Data.Hot || added.Data.BaseURL != up.URL {
		t.Fatalf("registration lost fields: %+v", added.Data)
	}
	// The platform is routable at once: it appears in the catalog, in account
	// validation and in the console's platform list without a restart.
	found := false
	for _, p := range providerCatalog(t, s) {
		if p.ID == "nova" {
			found = true
		}
	}
	if !found {
		t.Fatal("new platform missing from catalog")
	}
	if !config.Registered("nova") {
		t.Fatal("account validation does not accept the new platform")
	}
	if config.DefaultBaseURL("nova", "") != up.URL {
		t.Fatalf("default endpoint not declared: %q", config.DefaultBaseURL("nova", ""))
	}
	// An account on the platform (Base URL omitted, so the platform default is
	// used) can be created, discovered and routed immediately.
	w = adminCall(s, "POST", "/admin/v1/accounts", `{"id":"nova-1","provider":"nova","api_key":"nova-key","name":"nova account"}`)
	if w.Code != 201 {
		t.Fatalf("account on hot platform: %d %s", w.Code, w.Body)
	}
	a, err := s.Pool.Account("nova-1")
	if err != nil || a.BaseURL != up.URL {
		t.Fatalf("account base url: %+v %v", a, err)
	}
	s.Refresh(context.Background())
	w = call(s, "GET", "/v1/models", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hot-model") {
		t.Fatalf("catalog after registration: %d %s", w.Code, w.Body)
	}
	w = call(s, "POST", "/providers/nova/v1/chat/completions", `{"model":"hot-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok"`) {
		t.Fatalf("pinned request through hot platform: %d %s", w.Code, w.Body)
	}
	// The registration is persisted, so a restart rebinds the platform and its
	// accounts (including the default endpoint).
	rawCfg, err := os.ReadFile(s.Config.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadConfigDoc(t, string(rawCfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.ProviderOverrides) != 1 || reloaded.ProviderOverrides[0].ID != "nova" || reloaded.ProviderOverrides[0].BaseURL != up.URL {
		t.Fatalf("persisted overrides: %+v", reloaded.ProviderOverrides)
	}
}

func TestRemoveProviderUnbindsPlatform(t *testing.T) {
	s, up := newProviderGateway(t)
	body := fmt.Sprintf(`{"id":"nova","name":"Nova","base_url":%q}`, up.URL)
	if w := adminCall(s, "POST", "/admin/v1/providers", body); w.Code != 201 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	if w := adminCall(s, "POST", "/admin/v1/accounts", `{"id":"nova-1","provider":"nova","api_key":"nova-key"}`); w.Code != 201 {
		t.Fatalf("account: %d %s", w.Code, w.Body)
	}
	if w := adminCall(s, "DELETE", "/admin/v1/providers/nova", ""); w.Code != 200 {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if s.providers.Has("nova") || config.Registered("nova") {
		t.Fatal("removed platform still registered")
	}
	for _, a := range s.Pool.Accounts() {
		if a.Provider == "nova" {
			t.Fatal("account of a removed platform is still routed")
		}
	}
	// Request traffic and account administration both reject the platform.
	if w := call(s, "POST", "/providers/nova/v1/chat/completions", `{"model":"hot-model"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "unknown_provider") {
		t.Fatalf("pinned request to removed platform: %d %s", w.Code, w.Body)
	}
	if w := adminCall(s, "POST", "/admin/v1/accounts", `{"id":"nova-2","provider":"nova","api_key":"k2"}`); w.Code != 400 {
		t.Fatalf("account on removed platform: %d %s", w.Code, w.Body)
	}
	// Registering the same name again rebinds the stored account (it was kept,
	// not deleted) and restores the platform description.
	if w := adminCall(s, "POST", "/admin/v1/providers", body); w.Code != 201 {
		t.Fatalf("re-register: %d %s", w.Code, w.Body)
	}
	rebound := false
	for _, a := range s.Pool.Accounts() {
		if a.ID == "nova-1" {
			rebound = true
		}
	}
	if !rebound {
		t.Fatal("re-registering the platform did not rebind its stored account")
	}
}

func TestProviderAdminRejectsInvalidInput(t *testing.T) {
	s, up := newProviderGateway(t)
	cases := []struct {
		name, body string
		want       int
	}{
		{"reserved builtin", `{"id":"sensenova","base_url":"https://example.com"}`, 409},
		{"bad id", `{"id":"has space","base_url":"https://example.com"}`, 400},
		{"no id", `{"base_url":"https://example.com"}`, 400},
		{"path in base url", `{"id":"x","base_url":"https://example.com/v1"}`, 400},
		{"plain http remote", `{"id":"x","base_url":"http://example.com"}`, 400},
		{"bad auth", fmt.Sprintf(`{"id":"x","base_url":%q,"auth":"magic"}`, up.URL), 400},
		{"bad realm", fmt.Sprintf(`{"id":"x","base_url":%q,"realms":["eu"]}`, up.URL), 400},
		{"declares builtin", fmt.Sprintf(`{"id":"x","base_url":%q,"built_in":true}`, up.URL), 400},
	}
	for _, tc := range cases {
		if w := adminCall(s, "POST", "/admin/v1/providers", tc.body); w.Code != tc.want {
			t.Fatalf("%s: got %d want %d (%s)", tc.name, w.Code, tc.want, w.Body)
		}
	}
	// An unknown field is rejected rather than silently ignored.
	if w := adminCall(s, "POST", "/admin/v1/providers", fmt.Sprintf(`{"id":"y","base_url":%q,"bogus":1}`, up.URL)); w.Code != 400 {
		t.Fatalf("unknown field: %d %s", w.Code, w.Body)
	}
	if w := adminCall(s, "PUT", "/admin/v1/providers/nova", `{"name":"Nova"}`); w.Code != 404 {
		t.Fatalf("update of unknown platform: %d %s", w.Code, w.Body)
	}
}

func TestRegisterProviderRollsBackWithoutConfigFile(t *testing.T) {
	s, up := newProviderGateway(t)
	s.Config.SourcePath = "" // no config file: the registration cannot be persisted
	w := adminCall(s, "POST", "/admin/v1/providers", fmt.Sprintf(`{"id":"nova","base_url":%q}`, up.URL))
	// With no SourcePath the write-back is a no-op (same contract as the other
	// hot settings), so the platform stays active for this process.
	if w.Code != 201 || !s.providers.Has("nova") {
		t.Fatalf("in-memory registration without a config file: %d %s", w.Code, w.Body)
	}
	// A failed write-back rolls the activation back so the process never
	// advertises a registration a restart would forget. A malformed config
	// file makes every write-back fail deterministically.
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"listen":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Config.SourcePath = cfgPath
	w = adminCall(s, "POST", "/admin/v1/providers", fmt.Sprintf(`{"id":"broken","base_url":%q}`, up.URL))
	if w.Code != 500 || s.providers.Has("broken") || config.Registered("broken") {
		t.Fatalf("failed persistence must roll back: %d %s", w.Code, w.Body)
	}
	if config.DefaultBaseURL("broken", "") != "" {
		t.Fatal("rollback left the endpoint declaration behind")
	}
}

func TestUpdateProviderDescription(t *testing.T) {
	s, up := newProviderGateway(t)
	body := fmt.Sprintf(`{"id":"nova","name":"Nova","base_url":%q}`, up.URL)
	if w := adminCall(s, "POST", "/admin/v1/providers", body); w.Code != 201 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	w := adminCall(s, "PUT", "/admin/v1/providers/nova", `{"name":"Nova Pro","tagline":"改名后","id_prefix":"np"}`)
	if w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	var updated struct {
		Data PlatformInfo `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Data.Name != "Nova Pro" || updated.Data.Tagline != "改名后" || updated.Data.IDPrefix != "np" || updated.Data.BaseURL != up.URL {
		t.Fatalf("partial update mangled the description: %+v", updated.Data)
	}
	if w := adminCall(s, "PUT", "/admin/v1/providers/nova", `{"id":"other"}`); w.Code != 400 {
		t.Fatalf("renaming a platform must be rejected: %d %s", w.Code, w.Body)
	}
}

func TestProviderEndpointsRequireAdmin(t *testing.T) {
	s, _ := newProviderGateway(t)
	r := httptest.NewRequest("POST", "/admin/v1/providers", strings.NewReader(`{"id":"x","base_url":"https://example.com"}`))
	r.Header.Set("Authorization", "Bearer gateway-secret-at-least-16") // the client key, not the admin key
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("client key must not manage platforms: %d %s", rec.Code, rec.Body)
	}
}

func TestProviderRegistrationSurvivesRestart(t *testing.T) {
	s, up := newProviderGateway(t)
	body := fmt.Sprintf(`{"id":"nova","name":"Nova","base_url":%q,"realms":["intl"]}`, up.URL)
	if w := adminCall(s, "POST", "/admin/v1/providers", body); w.Code != 201 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	cfg, err := loadConfigDoc(t, mustRead(t, s.Config.SourcePath))
	if err != nil {
		t.Fatal(err)
	}
	// A fresh process replays the saved platform before validating accounts.
	c := cfg
	c.DBFile = filepath.Join(t.TempDir(), "restart.db")
	c.Accounts = []config.Account{{ID: "env-nova", Provider: "nova", APIKey: "env-key", MaxInFlight: 2}}
	replayed := New(c)
	if !replayed.providers.Has("nova") {
		t.Fatal("saved platform was not replayed at boot")
	}
	info, _ := replayed.providers.Info("nova")
	if info.Name != "Nova" || info.BaseURL != up.URL || len(info.Realms) != 1 || !info.Hot {
		t.Fatalf("replayed description: %+v", info)
	}
	// An env-declared account on the replayed platform validates and uses the
	// platform's default endpoint.
	if err := config.NormalizeAccount(&config.Account{ID: "env-nova", Provider: "nova", APIKey: "k"}); err != nil {
		t.Fatalf("account on replayed platform: %v", err)
	}
	if got := config.DefaultBaseURL("nova", "intl"); got != up.URL {
		t.Fatalf("replayed realm origin = %q", got)
	}
}

func TestGenericAdapterWireContract(t *testing.T) {
	var gotAuth, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = io.WriteString(w, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
	}))
	defer up.Close()
	g := provider.Generic{}
	models, err := g.Models(context.Background(), up.Client(), config.Account{BaseURL: up.URL, APIKey: "k"})
	if err != nil || len(models) != 2 || gotAuth != "Bearer k" || gotPath != "/v1/models" {
		t.Fatalf("generic discovery: %v %v %s %s", models, err, gotAuth, gotPath)
	}
	if _, err := g.Quota(context.Background(), up.Client(), config.Account{}); err == nil {
		t.Fatal("generic platforms have no documented quota API")
	}
	if !g.ModelUnavailable([]byte(`{"error":{"code":"model_not_found","message":"no such model"}}`)) {
		t.Fatal("model permission error not recognized")
	}
	if !g.ModelRateLimit([]byte(`{"error":{"code":"rate_limit_exceeded"}}`)) {
		t.Fatal("OpenAI rate limit not recognized")
	}
}
