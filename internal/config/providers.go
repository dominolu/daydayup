package config

import (
	"errors"
	"net/url"
	"strings"
	"sync"
)

// providerGate is the set of platform names currently accepted by account
// validation. It is guarded runtime state, not a constant: the admin console
// registers and removes platforms on a running gateway, and accounts are
// validated against whatever is registered at that moment.
var (
	registryMu      sync.RWMutex
	registeredNames []string
)

// SetRegisteredProviders replaces the accepted platform names (in listing
// order). The gateway calls it from the provider registry's change hook so
// config validation and the live registry can never disagree.
func SetRegisteredProviders(names []string) {
	registryMu.Lock()
	registeredNames = append([]string{}, names...)
	registryMu.Unlock()
}

// RegisteredProviders returns the accepted platform names in registration
// order (a copy; callers may retain it).
func RegisteredProviders() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return append([]string{}, registeredNames...)
}

// Registered reports whether provider is a registered platform adapter.
func Registered(provider string) bool {
	registryMu.RLock()
	defer registryMu.RUnlock()
	for _, p := range registeredNames {
		if p == provider {
			return true
		}
	}
	return false
}

// RealmProducts records the product values a realm-bearing platform accepts;
// a nil/empty product list means the platform has no product family. The
// gateway's default realm list is built in where the adapter lives.
var (
	realmProductMu    sync.RWMutex
	realmProducts     = map[string][]string{}
	realmHeaderChecks = map[string]func(Account) bool{}
)

// SetRealmPlatform declares platform as realm-aware: values RealmValues are
// the accepted realm values (a credentials realm check requires exactly one),
// and the optional looksCN derives the realm from the account's Base URL host
// when no explicit realm was stored. The built-in WorkBuddy adapter installs
// itself at boot; hot-registered platforms without this declaration are
// realm-agnostic.
func SetRealmPlatform(platform string, realmValues, products []string, looksCN func(Account) bool) {
	realmProductMu.Lock()
	defer realmProductMu.Unlock()
	realmProducts[platform] = append([]string{}, products...)
	if looksCN != nil {
		realmHeaderChecks[platform] = looksCN
	} else {
		delete(realmHeaderChecks, platform)
	}
	realmValuesMu.Lock()
	realmValuesByPlatform[platform] = append([]string{}, realmValues...)
	realmValuesMu.Unlock()
}

var (
	realmValuesMu         sync.RWMutex
	realmValuesByPlatform = map[string][]string{}
)

// RealmValues returns the realm values a realm-aware platform accepts.
func RealmValues(platform string) []string {
	realmValuesMu.RLock()
	defer realmValuesMu.RUnlock()
	return append([]string{}, realmValuesByPlatform[platform]...)
}

// RealmAware reports whether account validation applies the realm checks to
// the platform: a realm value is validated against RealmValues, and a single
// realm (WorkBuddy's CodeBuddy China endpoint) additionally requires uid and
// a bounded refresh token because that realm authenticates by OAuth.
func RealmAware(platform string) bool {
	realmValuesMu.RLock()
	defer realmValuesMu.RUnlock()
	return len(realmValuesByPlatform[platform]) > 0
}

// Products returns the client-identity values a platform accepts.
func Products(platform string) []string {
	realmProductMu.RLock()
	defer realmProductMu.RUnlock()
	return append([]string{}, realmProducts[platform]...)
}

// EndpointSpec declares where a platform lives: Origin is the default Base
// URL, and Realms maps a realm value to its own origin. The gateway installs
// the declaration from the live provider registry (adapter self-description
// or runtime registration), so config carries no per-platform URL table.
type EndpointSpec struct {
	Origin string            `json:"origin"`
	Realms map[string]string `json:"realms,omitempty"`
}

var (
	endpointMu   sync.RWMutex
	endpointSpec = map[string]EndpointSpec{}
)

// SetEndpoint declares the endpoint family of a platform.
func SetEndpoint(platform string, spec EndpointSpec) {
	endpointMu.Lock()
	endpointSpec[platform] = spec
	endpointMu.Unlock()
}

// Endpoint returns the declared endpoint family of a platform.
func Endpoint(platform string) EndpointSpec {
	endpointMu.RLock()
	defer endpointMu.RUnlock()
	return endpointSpec[platform]
}

// DefaultBaseURL exposes the upstream origin default for a provider and realm
// ("" picks the platform origin), used by the admin console catalog and
// account normalization. An unknown platform yields "".
func DefaultBaseURL(provider, realm string) string {
	spec := Endpoint(provider)
	if realm != "" {
		if origin := spec.Realms[realm]; origin != "" {
			return origin
		}
	}
	return spec.Origin
}

// RealmOf derives the realm from the explicit realm value or, failing that,
// the platform's host pattern for the account's Base URL. Unknown platforms
// and unknown values default to "intl".
func RealmOf(a *Account) string {
	if a.Realm == "cn" || a.Realm == "intl" {
		return a.Realm
	}
	realmProductMu.RLock()
	looksCN := realmHeaderChecks[a.Provider]
	realmProductMu.RUnlock()
	if looksCN != nil && looksCN(*a) {
		return "cn"
	}
	return "intl"
}

// DefaultProduct validates the product field of a realm-aware platform and
// fills its default (the first declared product, or "desktop").
func DefaultProduct(a Account) string {
	products := Products(a.Provider)
	if len(products) == 0 {
		return ""
	}
	for _, p := range products {
		if p == a.Product {
			return a.Product
		}
	}
	return products[0]
}

// ValidateBaseURL enforces the origin rules shared by account base URLs and
// platform endpoint declarations: an origin only (no path, query, fragment or
// user info) over HTTPS, with HTTP allowed for loopback testing.
func ValidateBaseURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))) {
		return errors.New("Base URL 必须为 HTTPS 源站地址，不包含 /v1 路径（本机测试允许 HTTP）")
	}
	return nil
}

// HostLooksCN reports whether a URL names a mainland-China endpoint family.
// It exists so platform declarations do not each grow their own copy.
func HostLooksCN(baseURL string, markers []string) bool {
	h := strings.ToLower(baseURL)
	for _, marker := range markers {
		if strings.Contains(h, marker) {
			return true
		}
	}
	return false
}
