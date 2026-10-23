package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"safuno/internal/redact"
)

// DefaultPoolWaitTimeoutSeconds bounds rate-limit waiting when the config does
// not set pool_wait_timeout_seconds.
const DefaultPoolWaitTimeoutSeconds = 120

type Account struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	Managed         bool     `json:"-"`
	Provider        string   `json:"provider"`
	Group           string   `json:"group,omitempty"` // routing group; empty means "default"
	BaseURL         string   `json:"base_url"`
	APIKeyEnv       string   `json:"api_key_env"`
	QuotaFeedURL    string   `json:"quota_feed_url,omitempty"`
	QuotaFeedKeyEnv string   `json:"quota_feed_key_env,omitempty"`
	QuotaFeedKey    string   `json:"-"`
	Models          []string `json:"models,omitempty"`
	MaxInFlight     int      `json:"max_in_flight"`
	Disabled        bool     `json:"disabled,omitempty"`
	APIKey          string   `json:"-"`
	// WorkBuddy / CodeBuddy OAuth accounts: APIKey carries the access token.
	RefreshToken    string `json:"refresh_token,omitempty"`     // OAuth refresh token, rotated by upstream
	RefreshTokenEnv string `json:"refresh_token_env,omitempty"` // environment variable holding the refresh token
	UID             string `json:"uid,omitempty"`               // X-User-Id; required for workbuddy
	UIDEnv          string `json:"uid_env,omitempty"`           // environment variable holding the UID
	Realm           string `json:"realm,omitempty"`             // workbuddy: "intl" or "cn" (defaults from base_url host)
	Product         string `json:"product,omitempty"`           // workbuddy identity: desktop|vscode|cli (default desktop)
	Domain          string `json:"domain,omitempty"`            // X-Domain override (defaults per realm)
	EnterpriseID    string `json:"enterprise_id,omitempty"`     // enterprise account (X-Enterprise-Id)
}

// SessionsConfig tunes the session archive inside the gateway database.
type SessionsConfig struct {
	// RetentionDays deletes archived body content of requests older than N
	// days (0 = keep forever). Records/metadata are never deleted.
	RetentionDays int `json:"retention_days,omitempty"`
	// MaxBodyMB keeps only the newest archived bodies within N MB of stored
	// body content, deleting the oldest overflow (0 = unlimited).
	MaxBodyMB int `json:"max_body_mb,omitempty"`
	// BodyFiles stores archived bodies on disk (one directory per session,
	// one encrypted file per request direction) with only an index in the
	// database. nil (default) enables file storage; set false to store body
	// BLOBs inside the database as before.
	BodyFiles *bool `json:"body_files,omitempty"`
}

// BodyFilesEnabled reports the effective body storage mode (default: files).
func (s SessionsConfig) BodyFilesEnabled() bool { return s.BodyFiles == nil || *s.BodyFiles }

// ShouldPrune reports whether any body retention policy is enabled.
func (s SessionsConfig) ShouldPrune() bool { return s.RetentionDays > 0 || s.MaxBodyMB > 0 }

type Config struct {
	InstanceName       string `json:"instance_name"`
	SessionStoreDir    string `json:"session_store_dir"`
	SessionStoreKeyEnv string `json:"session_store_key_env"`
	AccountStoreFile   string `json:"account_store_file"`
	AccountStoreKeyEnv string `json:"account_store_key_env"`
	AgentStoreFile     string `json:"agent_store_file,omitempty"`
	Listen             string `json:"listen"`
	// DBFile is the single local SQLite database holding accounts, pool state,
	// session archives, redaction rules and agent bindings. The legacy
	// per-domain storage fields below are kept only as migration inputs.
	DBFile                string `json:"db_file"`
	DBKeyEnv              string `json:"db_key_env,omitempty"`
	DefaultProvider       string `json:"default_provider"`
	DefaultGroup          string `json:"default_group,omitempty"` // routing group when the client sends none
	DefaultRealm          string `json:"default_realm,omitempty"` // workbuddy scheduling preference: intl|cn
	GatewayKeyEnv         string `json:"gateway_key_env"`
	AdminKeyEnv           string `json:"admin_key_env"`
	StateFile             string `json:"state_file"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds"`
	// PoolWaitTimeoutSeconds caps how much of the request budget may be spent
	// waiting for upstream capacity or rate-limit recovery. Without a separate
	// cap a 429 cools account selection for minutes and the wait silently
	// consumes the generation window, which the client then sees as a
	// truncated or cancelled stream. Zero means the default (120s); -1
	// disables the separate budget and keeps the legacy
	// request_timeout_seconds-only behavior.
	PoolWaitTimeoutSeconds int               `json:"pool_wait_timeout_seconds"`
	RefreshIntervalSeconds int               `json:"refresh_interval_seconds"`
	MaxRequestBytes        int64             `json:"max_request_bytes"`
	MaxAttempts            int               `json:"max_attempts"`
	MaxWaitingRequests     int               `json:"max_waiting_requests"`
	Routing                string            `json:"routing,omitempty"`       // global "round_robin" (default) | "smart"
	GroupRouting           map[string]string `json:"group_routing,omitempty"` // per-group override: group -> "round_robin" | "smart"; absent groups use Routing
	// ProviderOverrides holds operator-registered platforms (admin console →
	// 平台). They are replayed onto the provider registry at boot, so a
	// platform added without restarting survives one, including its default
	// endpoint for accounts that omit base_url.
	ProviderOverrides      []ProviderOverride `json:"provider_overrides,omitempty"`
	Accounts               []Account         `json:"accounts"`
	Guards                 Guards            `json:"guards,omitempty"`
	Sessions               SessionsConfig    `json:"sessions,omitempty"`
	Redaction              *redact.Config    `json:"redaction,omitempty"`
	Agents                 AgentsConfig      `json:"agents,omitempty"`
	Bridges                BridgesConfig     `json:"bridges,omitempty"`
	Pricing                PricingConfig     `json:"pricing,omitempty"`
	Middleware             MiddlewareConfig  `json:"middleware,omitempty"`
	// ModelRename maps, per platform, an upstream model id to the client-facing
	// canonical name. Routing, GET /v1/models and response model fields use the
	// canonical name, while each platform still receives the id it advertises.
	// Example: {"sensenova": {"deepseek-flash": "deepseek-v4.1-flash-1017"}}.
	ModelRename map[string]map[string]string `json:"model_rename,omitempty"`
	Prewarm     PrewarmConfig                `json:"prewarm,omitempty"`
	Intent      IntentConfig                 `json:"intent,omitempty"`
	GatewayKey  string                       `json:"-"`
	AdminKey    string                       `json:"-"`
	SourcePath  string                       `json:"-"`
}

// ProviderOverride is a persisted description of an operator-registered
// platform. It carries display metadata, the default endpoint and the realm
// vocabulary; the adapter code itself is always provider.Generic in-process.
type ProviderOverride struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Tagline     string   `json:"tagline,omitempty"`
	Logo        string   `json:"logo,omitempty"`
	IDPrefix    string   `json:"id_prefix,omitempty"`
	BaseURL     string   `json:"base_url,omitempty"`
	Credential  string   `json:"credential,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Hint        string   `json:"hint,omitempty"`
	Auth        string   `json:"auth,omitempty"`
	UIDField    string   `json:"uid_field,omitempty"`
	Realms      []string `json:"realms,omitempty"`
	Quota       string   `json:"quota,omitempty"`
	// Adapter names the adapter implementation the platform runs; only
	// "generic" (the OpenAI-compatible adapter) is available in-process.
	Adapter string `json:"adapter,omitempty"`
}

// AgentsConfig controls the agent-adaptation layer (M1/M2 of the product
// plan): which local agents may be wired to the gateway, the default write
// mode, and the base URL written into agent configs.
type AgentsConfig struct {
	Enabled    bool   `json:"enabled,omitempty"`     // master switch; agents page off when false
	WriteMode  string `json:"write_mode,omitempty"`  // "auto" (default) | "snippet"
	GatewayURL string `json:"gateway_url,omitempty"` // origin written into agent configs (defaults to http://<listen>)
}

// BridgesConfig turns the optional protocol bridges on (M2). Both are off by
// default: the gateway then keeps its current passthrough behavior.
type BridgesConfig struct {
	Messages  bool `json:"messages,omitempty"`  // /v1/messages (Anthropic) <-> chat completions
	Responses bool `json:"responses,omitempty"` // /v1/responses <-> chat completions
}

// PricingConfig is the optional personal cost table: per-model prices in CNY
// per 1M tokens (input / output / cache read / cache write). WorkBuddy models
// fall back to their official credit multiplier (marked estimated); models
// with no price anywhere are shown as unpriced rather than guessed.
type PricingConfig map[string]ModelPrice // ModelPrice is one model's cost rows. Nil fields mean "no price": such calls
// are counted but never included in a cost total (the UI marks them unpriced).
type ModelPrice struct {
	Input     *float64 `json:"input,omitempty"`
	Output    *float64 `json:"output,omitempty"`
	CacheRead *float64 `json:"cache_read,omitempty"`
	// Estimated credits multiplier (WorkBuddy catalog) when the model has no
	// official token price; the UI marks the resulting cost as an estimate.
	CreditMultiplier *float64 `json:"credit_multiplier,omitempty"`
}

// MiddlewareConfig is the M4 gateway middleware layer (New API semantics):
// request-side model aliasing and system-prompt injection, plus response-side
// reasoning-content handling. All pieces are off when empty.
type MiddlewareConfig struct {
	// ModelMap renames a client model id before it goes upstream, e.g.
	// {"fast": "deepseek-chat"} — the request body model is rewritten.
	ModelMap map[string]string `json:"model_map,omitempty"`
	// SystemPrompt injects a system message into matching requests. match_model
	// is the client model id (empty = all); match_agent is a comma-separated
	// list of client identities from session detection (empty = all).
	SystemPrompt []SystemPromptRule `json:"system_prompt,omitempty"`
	// ThinkTags mirrors New API's think_tags: strip drops reasoning content
	// from Chat passthrough responses (streaming and non-streaming).
	ThinkTags *ThinkTagsConfig `json:"think_tags,omitempty"`
}

// SystemPromptRule is one injection rule.
type SystemPromptRule struct {
	MatchModel string `json:"match_model,omitempty"`
	MatchAgent string `json:"match_agent,omitempty"`
	Text       string `json:"text"`
}

// ThinkTagsConfig enables the reasoning drop for the Chat passthrough path.
type ThinkTagsConfig struct {
	Strip bool `json:"strip,omitempty"`
}

// PrewarmConfig (M4): at each quota-window reset hour the gateway issues one
// light request per eligible account so its window (e.g. WorkBuddy credits)
// is opened before real traffic arrives.
type PrewarmConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	Hour    *int `json:"hour,omitempty"`    // local hour (0-23); nil defaults to 4 (WorkBuddy window reset)
	Timeout int  `json:"timeout,omitempty"` // per-account timeout seconds (default 30)
}

// IntentConfig (M4 intent routing): a tiny classifier model reads each new
// turn and routes the request to a cheap or a strong model. classifier_model
// must be a concrete model (never a group); on any classifier failure the
// request falls back to strong_models[0] (safe by default).
type IntentConfig struct {
	Enabled          bool     `json:"enabled,omitempty"`
	ClassifierModel  string   `json:"classifier_model,omitempty"`
	SimpleModels     []string `json:"simple_models,omitempty"`
	StrongModels     []string `json:"strong_models,omitempty"`
	Models           []string `json:"models,omitempty"` // client model ids that trigger classification (default ["intent"])
	MaxTokens        int      `json:"max_tokens,omitempty"`
	SystemPromptText string   `json:"system_prompt,omitempty"`
}

// Guards are account-level daily usage guards (single gateway process).
// A zero value disables the corresponding guard. Counters reset at the local
// reset_hour (default 0).
type Guards struct {
	ReserveCredits float64 `json:"reserve_credits,omitempty"`        // skip when fresh credits fall below this
	DailyTokens    int64   `json:"daily_tokens,omitempty"`           // total daily token budget for an account
	DailyCredits   float64 `json:"daily_credits,omitempty"`          // estimated daily credit budget (paid models only)
	PerModelTokens int64   `json:"per_model_daily_tokens,omitempty"` // per-model daily token budget
	ResetHour      int     `json:"reset_hour,omitempty"`             // local hour the daily window rolls over (0-23)
}

func Load(path string) (Config, error) {
	c := Config{SessionStoreDir: "data/sessions", SessionStoreKeyEnv: "GATEWAY_SESSION_STORE_KEY", InstanceName: "Safuno 安诺网关", AccountStoreFile: "data/accounts.enc", AccountStoreKeyEnv: "GATEWAY_ACCOUNT_STORE_KEY", AgentStoreFile: "data/agents.json", Listen: "127.0.0.1:8080", DefaultProvider: "sensenova", GatewayKeyEnv: "GATEWAY_API_KEY", AdminKeyEnv: "GATEWAY_ADMIN_KEY", StateFile: "data/state.json", RequestTimeoutSeconds: 300, RefreshIntervalSeconds: 60, MaxRequestBytes: 32 << 20, MaxAttempts: 3, DBFile: "data/gateway.db", DBKeyEnv: "GATEWAY_DB_KEY"}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	c.SourcePath, err = filepath.Abs(path)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("config must contain exactly one JSON object")
	}
	if c.MaxWaitingRequests == 0 {
		c.MaxWaitingRequests = 128
	}
	if c.MaxWaitingRequests < 1 || c.MaxWaitingRequests > 10000 {
		return c, fmt.Errorf("max_waiting_requests must be between 1 and 10000")
	}
	if c.PoolWaitTimeoutSeconds == 0 {
		c.PoolWaitTimeoutSeconds = DefaultPoolWaitTimeoutSeconds
	}
	if c.PoolWaitTimeoutSeconds < -1 {
		return c, fmt.Errorf("pool_wait_timeout_seconds must be -1 (disabled) or a positive number of seconds")
	}
	c.GatewayKey = os.Getenv(c.GatewayKeyEnv)
	c.AdminKey = os.Getenv(c.AdminKeyEnv)
	if len(c.GatewayKey) < 16 || len(c.AdminKey) < 16 || c.GatewayKey == c.AdminKey {
		return c, fmt.Errorf("gateway/admin environment keys must be distinct and at least 16 characters")
	}
	if c.Listen == "" || c.RequestTimeoutSeconds < 1 || c.RefreshIntervalSeconds < 1 || c.MaxRequestBytes < 1 || c.MaxRequestBytes > 1<<30 || c.MaxAttempts < 1 || c.MaxAttempts > 20 {
		return c, fmt.Errorf("invalid server limits or empty account pool")
	}
	if c.DefaultRealm != "" && c.DefaultRealm != "intl" && c.DefaultRealm != "cn" {
		return c, fmt.Errorf("default_realm must be intl or cn")
	}
	if c.DefaultGroup != "" {
		group, err := NormalizeGroup(c.DefaultGroup)
		if err != nil {
			return c, err
		}
		c.DefaultGroup = group
	}
	if c.Guards.ResetHour < 0 || c.Guards.ResetHour > 23 {
		return c, fmt.Errorf("guards.reset_hour must be between 0 and 23")
	}
	if c.Guards.ReserveCredits < 0 || c.Guards.DailyTokens < 0 || c.Guards.DailyCredits < 0 || c.Guards.PerModelTokens < 0 {
		return c, fmt.Errorf("guards must be nonnegative")
	}
	if c.Agents.WriteMode != "" && c.Agents.WriteMode != "auto" && c.Agents.WriteMode != "snippet" {
		return c, fmt.Errorf("agents.write_mode must be auto or snippet")
	}
	if c.Routing != "" && c.Routing != "round_robin" && c.Routing != "smart" {
		return c, fmt.Errorf("routing must be round_robin or smart")
	}
	for group, mode := range c.GroupRouting {
		if _, err := NormalizeGroup(group); err != nil {
			return c, fmt.Errorf("group_routing %q: %v", group, err)
		}
		if mode != "" && mode != "round_robin" && mode != "smart" {
			return c, fmt.Errorf("group_routing %q: must be round_robin or smart", group)
		}
	}
	for client, upstream := range c.Middleware.ModelMap {
		if strings.TrimSpace(client) == "" || strings.TrimSpace(upstream) == "" {
			return c, fmt.Errorf("middleware.model_map keys and values must be non-empty")
		}
	}
	rename, err := NormalizeModelRename(c.ModelRename)
	if err != nil {
		return c, err
	}
	c.ModelRename = rename
	for _, rule := range c.Middleware.SystemPrompt {
		if strings.TrimSpace(rule.Text) == "" {
			return c, fmt.Errorf("middleware.system_prompt rules need non-empty text")
		}
	}
	if c.Prewarm.Enabled {
		if c.Prewarm.Hour != nil && (*c.Prewarm.Hour < 0 || *c.Prewarm.Hour > 23) {
			return c, fmt.Errorf("prewarm.hour must be between 0 and 23")
		}
	}
	if c.Intent.Enabled {
		if c.Intent.ClassifierModel == "" || len(c.Intent.SimpleModels) == 0 || len(c.Intent.StrongModels) == 0 {
			return c, fmt.Errorf("intent requires classifier_model, simple_models and strong_models")
		}
		if len(c.Intent.Models) == 0 {
			c.Intent.Models = []string{"intent"}
		} else {
			for _, m := range c.Intent.Models {
				if m == c.Intent.ClassifierModel {
					return c, fmt.Errorf("intent.classifier_model must not be one of intent.models (recursion)")
				}
			}
		}
		if c.Intent.MaxTokens <= 0 || c.Intent.MaxTokens > 16 {
			c.Intent.MaxTokens = 8
		}
	}
	if c.Agents.WriteMode == "" {
		c.Agents.WriteMode = "auto"
	}
	if c.Agents.GatewayURL == "" {
		c.Agents.GatewayURL = "http://" + c.Listen
	}
	if c.DBFile == "" {
		c.DBFile = "data/gateway.db"
	}
	if c.DBKeyEnv == "" {
		c.DBKeyEnv = "GATEWAY_DB_KEY"
	}
	if c.Redaction != nil {
		if err := c.Redaction.Normalize(); err != nil {
			return c, err
		}
		if c.Redaction.StoreFile == "" {
			return c, fmt.Errorf("redaction store_file is required")
		}
	}
	seen := map[string]bool{}
	keys := map[string]bool{}
	found := false
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if a.Provider == "" {
			a.Provider = "sensenova"
		}
		if !Registered(a.Provider) {
			return c, fmt.Errorf("account %s: provider %q is not registered", a.ID, a.Provider)
		}
		if !regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`).MatchString(a.ID) || seen[a.ID] {
			return c, fmt.Errorf("invalid or duplicate account id")
		}
		seen[a.ID] = true
		if a.QuotaFeedURL != "" {
			q, e := url.Parse(a.QuotaFeedURL)
			if e != nil || q.Host == "" || q.User != nil || q.Fragment != "" || q.RawQuery != "" || (q.Scheme != "https" && !(q.Scheme == "http" && (q.Hostname() == "localhost" || q.Hostname() == "127.0.0.1" || q.Hostname() == "::1"))) {
				return c, fmt.Errorf("account %s: invalid quota_feed_url", a.ID)
			}
			if a.QuotaFeedKeyEnv != "" {
				a.QuotaFeedKey = os.Getenv(a.QuotaFeedKeyEnv)
				if a.QuotaFeedKey == "" || strings.ContainsAny(a.QuotaFeedKey, "\r\n") {
					return c, fmt.Errorf("account %s: missing quota feed credential", a.ID)
				}
			}
		}
		a.APIKey = os.Getenv(a.APIKeyEnv)
		if a.APIKey == "" || strings.ContainsAny(a.APIKey, "\r\n") {
			return c, fmt.Errorf("account %s: missing or invalid credential in %s", a.ID, a.APIKeyEnv)
		}
		if a.RefreshTokenEnv != "" {
			a.RefreshToken = os.Getenv(a.RefreshTokenEnv)
			if a.RefreshToken == "" || strings.ContainsAny(a.RefreshToken, "\r\n") {
				return c, fmt.Errorf("account %s: missing refresh token in %s", a.ID, a.RefreshTokenEnv)
			}
		}
		if a.UIDEnv != "" {
			a.UID = os.Getenv(a.UIDEnv)
			if a.UID == "" || strings.ContainsAny(a.UID, "\r\n") {
				return c, fmt.Errorf("account %s: missing uid in %s", a.ID, a.UIDEnv)
			}
		}
		if err := validateRealmProduct(a); err != nil {
			return c, fmt.Errorf("account %s: %w", a.ID, err)
		}
		if keys[a.BaseURL+"\x00"+a.APIKey+"\x00"+a.Provider] {
			return c, fmt.Errorf("account %s: duplicate upstream credential", a.ID)
		}
		keys[a.BaseURL+"\x00"+a.APIKey+"\x00"+a.Provider] = true
		if a.MaxInFlight == 0 {
			a.MaxInFlight = 4
		}
		if a.MaxInFlight < 1 {
			return c, fmt.Errorf("account %s: invalid concurrency", a.ID)
		}
		if err := NormalizeAccount(a); err != nil {
			return c, fmt.Errorf("account %s: %w", a.ID, err)
		}
		if a.Provider == c.DefaultProvider {
			found = true
		}
	}
	if len(c.Accounts) > 0 && !found && c.DefaultProvider != "sensenova" {
		return c, fmt.Errorf("default provider has no accounts")
	}
	return c, nil
}

// NormalizeGroup validates and canonicalizes a routing group name. The empty
// value means the default group ("default"). Groups bound the aggregated
// model→account pool: a request routed under group G only ever sees accounts
// whose group is G.
func NormalizeGroup(g string) (string, error) {
	g = strings.TrimSpace(g)
	if g == "" || g == "default" {
		return "default", nil
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`).MatchString(g) {
		return "", fmt.Errorf("group 只能包含字母、数字、点、下划线和短横线，长度 1–64")
	}
	return g, nil
}

// NormalizeAccount validates both environment-backed and UI-created accounts.
func NormalizeAccount(a *Account) error {
	if !regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`).MatchString(a.ID) {
		return fmt.Errorf("账号 ID 只能包含字母、数字、点、下划线和短横线，长度 1–64")
	}
	group, err := NormalizeGroup(a.Group)
	if err != nil {
		return err
	}
	a.Group = group
	if a.Name == "" {
		a.Name = a.ID
	}
	if len([]rune(a.Name)) > 80 || strings.ContainsAny(a.Name, "\r\n\x00") {
		return fmt.Errorf("账号名称不合法，最多 80 个字符")
	}
	if a.Provider == "" {
		a.Provider = "sensenova"
	}
	if !Registered(a.Provider) {
		return fmt.Errorf("当前仅支持 %s", strings.Join(RegisteredProviders(), "、"))
	}
	if a.BaseURL == "" {
		a.BaseURL = DefaultBaseURL(a.Provider, a.Realm)
	}
	u, e := url.Parse(a.BaseURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))) {
		return fmt.Errorf("Base URL 必须为 HTTPS 源站地址，不包含 /v1 路径（本机测试允许 HTTP）")
	}
	a.BaseURL = strings.TrimRight(a.BaseURL, "/")
	if err := validateRealmProduct(a); err != nil {
		return err
	}
	if a.APIKey == "" || len(a.APIKey) > 4096 || strings.ContainsAny(a.APIKey, "\r\n") {
		return fmt.Errorf("请填写有效的 API Key")
	}
	if a.MaxInFlight == 0 {
		a.MaxInFlight = 4
	}
	if a.MaxInFlight < 1 || a.MaxInFlight > 1000 {
		return fmt.Errorf("最大并发必须介于 1 和 1000")
	}
	if len(a.Models) > 256 {
		return fmt.Errorf("模型白名单最多 256 项")
	}
	for _, m := range a.Models {
		if len(m) == 0 || len(m) > 256 || strings.ContainsAny(m, "\r\n") {
			return fmt.Errorf("模型标识不合法")
		}
	}
	return nil
}

// validateRealmProduct validates and fills the platform-declared realm and
// client-identity fields. Platforms without a realm declaration ignore both
// fields; realm-aware platforms (WorkBuddy, and any platform that declares
// realms at runtime) are validated against their declared values. The
// single-realm WorkBuddy family authenticates by OAuth, so it additionally
// requires an uid and a bounded refresh token because credentials imported from
// an account export.
func validateRealmProduct(a *Account) error {
	values := RealmValues(a.Provider)
	if len(values) == 0 {
		a.Product = ""
		return nil
	}
	if a.Realm != "" {
		valid := false
		for _, v := range values {
			if v == a.Realm {
				valid = true
			}
		}
		if !valid {
			return fmt.Errorf("区域必须是 %s", strings.Join(values, " 或 "))
		}
	}
	products := Products(a.Provider)
	if a.Product != "" {
		valid := false
		for _, p := range products {
			if p == a.Product {
				valid = true
			}
		}
		if !valid {
			return fmt.Errorf("客户端身份必须是 %s", strings.Join(products, "、"))
		}
	}
	a.Realm = RealmOf(a)
	a.Product = DefaultProduct(*a)
	if a.Provider == "workbuddy" {
		if a.UID == "" {
			return fmt.Errorf("WorkBuddy 账号需要 uid（X-User-Id），请从 OAuth 导入或检查配置")
		}
		if len(a.UID) > 128 || strings.ContainsAny(a.UID, "\r\n\x00") {
			return fmt.Errorf("uid 不合法")
		}
		if len(a.RefreshToken) > 8192 || strings.ContainsAny(a.RefreshToken, "\r\n\x00") {
			return fmt.Errorf("refresh_token 不合法")
		}
	}
	return nil
}

// CanonicalModel returns the client-facing canonical name for a model id: the
// rename target configured on any platform, or the id itself when no platform
// renames it. Renames are validated so a source id has at most one target
// across platforms; the provider order only makes the lookup deterministic.
func CanonicalModel(rename map[string]map[string]string, model string) string {
	if len(rename) == 0 || model == "" {
		return model
	}
	providers := make([]string, 0, len(rename))
	for provider := range rename {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		if canonical := rename[provider][model]; canonical != "" {
			return canonical
		}
	}
	return model
}

// UpstreamModel returns the model id a given platform must receive for the
// canonical client model: the renamed source id when that platform renames one
// of its ids to canonical, otherwise canonical itself (the platform's native
// id). When several sources on one platform map to the same canonical, the
// lexicographically first source wins deterministically.
func UpstreamModel(rename map[string]map[string]string, provider, canonical string) string {
	if len(rename) == 0 || canonical == "" {
		return canonical
	}
	entries := rename[provider]
	if len(entries) == 0 {
		return canonical
	}
	source := ""
	for upstream, target := range entries {
		if target == canonical && (source == "" || upstream < source) {
			source = upstream
		}
	}
	if source == "" {
		return canonical
	}
	return source
}

// NormalizeModelRename validates the per-platform rename table and returns a
// trimmed copy. Rejected: empty provider/source/target values, the same source
// mapping to different targets on different platforms, and chained renames
// (a target that is itself a renamed source), which would make canonical names
// order-dependent. Identity entries are dropped.
func NormalizeModelRename(raw map[string]map[string]string) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	targets := map[string]string{}
	for provider, entries := range raw {
		name := strings.TrimSpace(provider)
		if name == "" {
			return nil, fmt.Errorf("model_rename 的平台名不能为空")
		}
		next := map[string]string{}
		for source, target := range entries {
			from, to := strings.TrimSpace(source), strings.TrimSpace(target)
			if from == "" || to == "" {
				return nil, fmt.Errorf("model_rename[%s] 的原模型名与新模型名都不能为空", name)
			}
			if from == to {
				continue
			}
			if previous, ok := targets[from]; ok && previous != to {
				return nil, fmt.Errorf("模型 %q 在多个平台被改成不同名称（%q / %q）", from, previous, to)
			}
			targets[from] = to
			next[from] = to
		}
		if len(next) > 0 {
			out[name] = next
		}
	}
	for _, entries := range out {
		for _, target := range entries {
			if _, ok := targets[target]; ok {
				return nil, fmt.Errorf("模型改名不能链式（%q 已经是另一个改名的原模型名）", target)
			}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
