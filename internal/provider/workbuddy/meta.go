package workbuddy

import (
	"safuno/internal/config"
	wbp "safuno/internal/provider"
)

var _ wbp.Descriptor = WorkBuddy{}

// Client identity families the adapter impersonates. The gateway validates
// account.Product against these values (declared below), so the adapter owns
// its own vocabulary instead of config hard-coding WorkBuddy words.
const (
	// ProductDesktop is the WorkBuddy desktop client family (default).
	ProductDesktop = "desktop"
	// ProductVSCode is the VSCode plugin identity (X-Model-ID, gzip bodies).
	ProductVSCode = "vscode"
	// ProductCLI is the CLI identity (gzip bodies).
	ProductCLI = "cli"
)

// ProductValues lists every accepted client identity, in display order.
var ProductValues = []string{ProductDesktop, ProductVSCode, ProductCLI}

// realmNames lists the scheduling realms, in the order the console offers.
var realmNames = []string{"intl", "cn"}

// cnMarkers are Base URL hosts that belong to the mainland realm family.
var cnMarkers = []string{"copilot.tencent.com", "codebuddy.cn", "workbuddy.cn"}

func init() {
	// Declare WorkBuddy as realm- and product-bearing so account validation
	// accepts its realm/product vocabulary. config does not name WorkBuddy.
	config.SetRealmPlatform("workbuddy", realmNames, ProductValues, func(a config.Account) bool {
		return config.HostLooksCN(a.BaseURL, cnMarkers)
	})
	// The two realms are separate upstream origins (CN schedules against the
	// CodeBuddy China endpoints, and the realm lookup reads the base URL host).
	config.SetEndpoint("workbuddy", config.EndpointSpec{
		Origin: realms["intl"].origin,
		Realms: map[string]string{"intl": realms["intl"].origin, "cn": realms["cn"].origin},
	})
}

// defaultProduct validates an account's client identity for this adapter:
// an empty or unknown value means the default desktop family. Accounts
// normalized by config already carry a declared value; this keeps zero-value
// accounts (tests, legacy records) on the desktop identity.
func defaultProduct(a config.Account) string {
	for _, p := range ProductValues {
		if p == a.Product {
			return p
		}
	}
	return ProductDesktop
}

// PlatformInfo describes WorkBuddy / CodeBuddy for the admin console: OAuth
// login, an X-User-Id identity header and the intl/cn realms.
func (WorkBuddy) PlatformInfo() wbp.Info {
	return wbp.Info{
		ID:          "workbuddy",
		Name:        "WorkBuddy",
		Tagline:     "腾讯 CodeBuddy · OAuth 登录",
		Logo:        "logo-workbuddy",
		IDPrefix:    "wb",
		Credential:  "Access Token",
		Placeholder: "…",
		Hint:        "使用浏览器登录授权，或粘贴已有的 uid / access token。",
		Auth:        "oauth",
		UIDField:    "X-User-Id",
		Realms:      append([]string{}, realmNames...),
		Quota:       "snapshot",
	}
}
