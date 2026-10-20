package config

import "strings"

// RegisteredProviders is the set of platform adapters the gateway can host.
// Extending a platform requires an internal/provider.Provider implementation,
// registration in gateway.New and an entry here.
var RegisteredProviders = []string{"sensenova", "workbuddy", "tokenplan"}

// Registered reports whether provider is a registered platform adapter.
func Registered(provider string) bool {
	for _, p := range RegisteredProviders {
		if p == provider {
			return true
		}
	}
	return false
}

// DefaultBaseURL exposes the upstream origin default for a provider, used by
// the admin console catalog and account normalization.
func DefaultBaseURL(provider string) string { return defaultBaseURL(provider, "") }

// defaultBaseURL returns the upstream origin default for a provider/realm.
func defaultBaseURL(provider, realm string) string {
	switch provider {
	case "workbuddy":
		if realm == "cn" {
			return "https://copilot.tencent.com"
		}
		return "https://www.workbuddy.ai"
	case "tokenplan":
		// Origin only: OpenAI-compatible endpoints live under /v1, so the
		// gateway appends the client path unchanged.
		return "https://discovery-api.intern-ai.org.cn"
	default:
		return "https://token.sensenova.cn"
	}
}

// RealmOf derives the WorkBuddy realm from the explicit realm value or the
// base URL host. Unknown values default to the international realm.
func RealmOf(a *Account) string {
	if a.Realm == "cn" || a.Realm == "intl" {
		return a.Realm
	}
	h := strings.ToLower(a.BaseURL)
	for _, marker := range []string{"copilot.tencent.com", "codebuddy.cn", "workbuddy.cn"} {
		if strings.Contains(h, marker) {
			return "cn"
		}
	}
	return "intl"
}

// WorkBuddy products: which client identity family the gateway impersonates.
const (
	ProductDesktop = "desktop"
	ProductVSCode  = "vscode"
	ProductCLI     = "cli"
)

// DefaultProduct validates the WorkBuddy product field and fills the default.
func DefaultProduct(a Account) string {
	switch a.Product {
	case ProductDesktop, ProductVSCode, ProductCLI:
		return a.Product
	default:
		return ProductDesktop
	}
}
