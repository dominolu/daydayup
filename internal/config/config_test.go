package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain declares the platform vocabulary the binary's built-ins install at
// boot (provider and adapter init functions). config cannot import providers
// (providers import config), so the declaration is repeated here.
func TestMain(m *testing.M) {
	registerTestPlatforms()
	os.Exit(m.Run())
}

func registerTestPlatforms() {
	SetRegisteredProviders([]string{"sensenova", "workbuddy", "tokenplan"})
	SetEndpoint("sensenova", EndpointSpec{Origin: "https://token.sensenova.cn"})
	SetEndpoint("tokenplan", EndpointSpec{Origin: "https://discovery-api.intern-ai.org.cn"})
	SetEndpoint("workbuddy", EndpointSpec{Origin: "https://www.workbuddy.ai"})
	// WorkBuddy's CN base-URL pattern is part of its adapter declaration; it
	// is reproduced here because config cannot import the adapter package.
	SetRealmPlatform("workbuddy", []string{"intl", "cn"}, []string{"desktop", "vscode", "cli"}, func(a Account) bool {
		return HostLooksCN(a.BaseURL, []string{"copilot.tencent.com", "codebuddy.cn", "workbuddy.cn"})
	})
}

func TestLoadValidation(t *testing.T) {
	t.Setenv("GATEWAY_API_KEY", "client-key-at-least-16")
	t.Setenv("GATEWAY_ADMIN_KEY", "admin-key-at-least-16")
	t.Setenv("SENSE_TEST_KEY", "sk-test")
	for _, tt := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", `{"accounts":[{"id":"a","api_key_env":"SENSE_TEST_KEY"}]}`, true},
		{"web-bootstrap", `{"accounts":[]}`, true},
		{"empty-workbuddy-bootstrap", `{"accounts":[],"default_provider":"workbuddy"}`, true},
		{"waiting-limit", `{"accounts":[],"max_waiting_requests":32}`, true},
		{"bad-waiting-limit", `{"accounts":[],"max_waiting_requests":-1}`, false},
		{"oversized-waiting-limit", `{"accounts":[],"max_waiting_requests":10001}`, false},
		{"bad-origin", `{"accounts":[{"id":"a","api_key_env":"SENSE_TEST_KEY","base_url":"https://example.com/v1"}]}`, false},
		{"plaintext-credential", `{"accounts":[{"id":"a","api_key":"secret"}]}`, false},
		{"unknown-provider", `{"accounts":[{"id":"a","api_key_env":"SENSE_TEST_KEY","provider":"other"}]}`, false},
		{"duplicate-key", `{"accounts":[{"id":"a","api_key_env":"SENSE_TEST_KEY"},{"id":"b","api_key_env":"SENSE_TEST_KEY"}]}`, false},
		{"group-routing-valid", `{"accounts":[],"group_routing":{"default":"smart","worker-a":"round_robin"}}`, true},
		{"group-routing-bad-mode", `{"accounts":[],"group_routing":{"default":"weird"}}`, false},
		{"group-routing-bad-name", `{"accounts":[],"group_routing":{"bad name!":"smart"}}`, false},
		{"extra-json", `{"accounts":[{"id":"a","api_key_env":"SENSE_TEST_KEY"}]} {}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.body), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t err=%v", tt.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "sk-test") {
				t.Fatal("secret in error")
			}
			if tt.valid && len(c.Accounts) > 0 && c.Accounts[0].MaxInFlight != 4 {
				t.Fatal("default concurrency missing")
			}
		})
	}
}

func TestLoadRedactionValidation(t *testing.T) {
	t.Setenv("GATEWAY_API_KEY", "client-key-at-least-16")
	t.Setenv("GATEWAY_ADMIN_KEY", "admin-key-at-least-16")
	for _, tt := range []struct {
		name, body string
		valid      bool
	}{
		{"disabled-ok", `{"accounts":[],"redaction":{"enabled":false}}`, true},
		{"enabled-ok", `{"accounts":[],"redaction":{"enabled":true}}`, true},
		{"bad-regex", `{"accounts":[],"redaction":{"enabled":true,"custom_words":{"X":["re:["]}}}`, false},
		{"unknown-disabled-rule", `{"accounts":[],"redaction":{"enabled":true,"disabled_rules":["NOPE"]}}`, false},
		{"custom-ok", `{"accounts":[],"redaction":{"enabled":true,"custom_words":{"CODENAME":["鹰眼系统"]},"disabled_rules":["EMAIL"]}}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t err=%v", tt.valid, err)
			}
		})
	}
}

func TestWorkBuddyAccountConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GATEWAY_API_KEY", "gateway-secret-at-least-16")
	t.Setenv("GATEWAY_ADMIN_KEY", "admin-secret-at-least-16")
	t.Setenv("WB_ACCESS", "access-token")
	t.Setenv("WB_REFRESH", "refresh-token")
	t.Setenv("WB_UID", "uid-123")
	path := dir + "/config.json"
	cfg := `{
	  "listen": "127.0.0.1:8080",
	  "default_provider": "workbuddy",
	  "default_realm": "cn",
	  "gateway_key_env": "GATEWAY_API_KEY",
	  "admin_key_env": "GATEWAY_ADMIN_KEY",
	  "account_store_file": "` + dir + `/accounts.enc",
	  "guards": {"reserve_credits": 10, "daily_tokens": 10000, "reset_hour": 4},
	  "accounts": [{
	    "id": "wb-1",
	    "provider": "workbuddy",
	    "api_key_env": "WB_ACCESS",
	    "refresh_token_env": "WB_REFRESH",
	    "uid_env": "WB_UID",
	    "realm": "intl",
	    "product": "cli"
	  }]
	}`
	if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.DefaultRealm != "cn" {
		t.Fatalf("default realm %q", c.DefaultRealm)
	}
	a := c.Accounts[0]
	if a.APIKey != "access-token" || a.RefreshToken != "refresh-token" || a.UID != "uid-123" {
		t.Fatalf("env tokens not resolved: %+v", a)
	}
	if a.Realm != "intl" || a.Product != "cli" {
		t.Fatalf("realm/product: %+v", a)
	}
	if c.Guards.ResetHour != 4 || c.Guards.DailyTokens != 10000 {
		t.Fatalf("guards: %+v", c.Guards)
	}
}

func TestWorkBuddyConfigValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GATEWAY_API_KEY", "gateway-secret-at-least-16")
	t.Setenv("GATEWAY_ADMIN_KEY", "admin-secret-at-least-16")
	t.Setenv("WB_ACCESS", "access")
	t.Setenv("WB_UID", "uid-1")
	t.Setenv("WB_BAD_UID", "")
	write := func(accounts string) error {
		path := dir + "/cfg.json"
		cfg := `{"listen":"127.0.0.1:8080","default_provider":"workbuddy","gateway_key_env":"GATEWAY_API_KEY","admin_key_env":"GATEWAY_ADMIN_KEY","account_store_file":"` + dir + `/a.enc","accounts":[` + accounts + `]}`
		if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		return err
	}
	if err := write(`{"id":"wb-1","provider":"workbuddy","api_key_env":"WB_ACCESS","uid_env":"WB_UID","realm":"intl"}`); err != nil {
		t.Fatalf("valid account rejected: %v", err)
	}
	if err := write(`{"id":"wb-1","provider":"workbuddy","api_key_env":"WB_ACCESS"}`); err == nil {
		t.Fatal("workbuddy account without uid must be rejected")
	}
	if err := write(`{"id":"wb-1","provider":"workbuddy","api_key_env":"WB_ACCESS","uid_env":"WB_UID","realm":"eu"}`); err == nil {
		t.Fatal("invalid realm must be rejected")
	}
	if err := write(`{"id":"wb-1","provider":"workbuddy","api_key_env":"WB_ACCESS","uid_env":"WB_UID","product":"bogus"}`); err == nil {
		t.Fatal("invalid product must be rejected")
	}
}

func TestGroupNormalization(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "default", false},
		{"default", "default", false},
		{"shared", "shared", false},
		{"  premium  ", "premium", false},
		{"a.b_c-d", "a.b_c-d", false},
		{"bad group!", "", true},
		{"", "default", false},
	}
	for _, tt := range cases {
		got, err := NormalizeGroup(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("NormalizeGroup(%q) accepted", tt.in)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Fatalf("NormalizeGroup(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	// Account group is normalized by NormalizeAccount.
	a := Account{ID: "a", Group: "  team-b  ", APIKey: "local-test-key-abc123"}
	if err := NormalizeAccount(&a); err != nil || a.Group != "team-b" {
		t.Fatalf("account group: %q %v", a.Group, err)
	}
	// default_group is normalized by Load.
	t.Setenv("GATEWAY_API_KEY", "client-key-at-least-16")
	t.Setenv("GATEWAY_ADMIN_KEY", "admin-key-at-least-16")
	t.Setenv("SENSE_TEST_KEY", "sk-test")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"accounts":[{"id":"a","api_key_env":"SENSE_TEST_KEY"}],"default_group":" team-a "}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.DefaultGroup != "team-a" {
		t.Fatalf("default_group load: %q %v", cfg.DefaultGroup, err)
	}
	if cfg.Accounts[0].Group != "default" {
		t.Fatalf("account group default: %q", cfg.Accounts[0].Group)
	}
}

// Pool waiting shares the request budget with generation; without a separate
// cap a rate-limited account can consume the whole window. The budget must be
// explicit, bounded and configurable (including disabled for legacy setups).
func TestPoolWaitBudgetDefaultAndValidation(t *testing.T) {
	t.Setenv("GATEWAY_API_KEY", "client-key-at-least-16")
	t.Setenv("GATEWAY_ADMIN_KEY", "admin-key-at-least-16")
	for _, tt := range []struct {
		name, body string
		valid      bool
		want       int
	}{
		{"default", `{"accounts":[]}`, true, DefaultPoolWaitTimeoutSeconds},
		{"explicit", `{"accounts":[],"pool_wait_timeout_seconds":30}`, true, 30},
		{"disabled", `{"accounts":[],"pool_wait_timeout_seconds":-1}`, true, -1},
		{"invalid", `{"accounts":[],"pool_wait_timeout_seconds":-2}`, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.body), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t err=%v", tt.valid, err)
			}
			if tt.valid && c.PoolWaitTimeoutSeconds != tt.want {
				t.Fatalf("pool_wait_timeout_seconds=%d want %d", c.PoolWaitTimeoutSeconds, tt.want)
			}
		})
	}
}
