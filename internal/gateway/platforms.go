package gateway

import "daydayup/internal/config"

// PlatformInfo is the admin-console description of a registered platform.
//
// The console renders the platform chooser from this catalog instead of a
// hard-coded list, so adding a platform means: implement provider.Provider,
// register it in gateway.New, add "name" to config.RegisteredProviders and one
// entry to the metadata table below. The UI then shows the new tile with no
// front-end change at all.
type PlatformInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Tagline     string `json:"tagline"`
	Logo        string `json:"logo"`       // sprite symbol id; "" falls back to a letter tile
	IDPrefix    string `json:"id_prefix"`  // prefix for generated account ids
	BaseURL     string `json:"base_url"`   // default upstream origin
	Credential  string `json:"credential"` // credential field label
	Placeholder string `json:"placeholder"`
	Hint        string `json:"hint"`         // one-line help under the credential field
	Auth        string `json:"auth"`         // "api_key" or "oauth"
	RequiresUID bool   `json:"requires_uid"` // extra X-User-Id field (WorkBuddy)
	Quota       string `json:"quota"`        // "snapshot" or "unsupported"
}

// platformMetadata is the display table for known platforms. It intentionally
// does not gate registration: an unknown registered provider still renders
// with a generated tile so a new adapter is usable before this table is
// extended.
var platformMetadata = map[string]PlatformInfo{
	"sensenova": {
		Name:        "SenseNova",
		Tagline:     "商汤日日新 · 积分额度",
		Logo:        "logo-sensenova",
		IDPrefix:    "sense",
		Credential:  "API Key",
		Placeholder: "sk-…",
		Hint:        "在 SenseNova 控制台创建 API Key 后粘贴到这里。",
		Auth:        "api_key",
		Quota:       "snapshot",
	},
	"workbuddy": {
		Name:        "WorkBuddy",
		Tagline:     "腾讯 CodeBuddy · OAuth 登录",
		Logo:        "logo-workbuddy",
		IDPrefix:    "wb",
		Credential:  "Access Token",
		Placeholder: "…",
		Hint:        "使用浏览器登录授权，或粘贴已有的 uid / access token。",
		Auth:        "oauth",
		RequiresUID: true,
		Quota:       "snapshot",
	},
	"tokenplan": {
		Name:        "Intern-ai",
		Tagline:     "书生·端砚 · 墨点额度",
		Logo:        "logo-tokenplan",
		IDPrefix:    "intern",
		Credential:  "API Key",
		Placeholder: "…",
		Hint:        "在 Intern-ai（TokenPlan）控制台创建 API Key（墨点计费，单人最多 10 把）。",
		Auth:        "api_key",
		Quota:       "unsupported",
	},
}

// platformCatalog returns the console catalog for every registered provider,
// ordered like config.RegisteredProviders so the tiles stay stable.
func platformCatalog() []PlatformInfo {
	out := make([]PlatformInfo, 0, len(config.RegisteredProviders))
	for _, id := range config.RegisteredProviders {
		info, ok := platformMetadata[id]
		if !ok {
			info = PlatformInfo{Name: id, Auth: "api_key", Quota: "unsupported"}
		}
		info.ID = id
		if info.BaseURL == "" {
			info.BaseURL = config.DefaultBaseURL(id)
		}
		out = append(out, info)
	}
	return out
}
