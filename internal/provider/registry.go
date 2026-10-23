package provider

import (
	"errors"
	"fmt"
	"sync"
)

// Registry maps provider names to their implementations. It is the extension
// point for additional upstream platforms: build a registry, register your
// providers, and hand it to the gateway instead of relying on the built-in
// defaults. The registry is live and safe for concurrent use: the admin
// console adds and removes platforms on a running gateway, so every reader
// (request pipeline, refresh loop, background tasks) observes changes without
// a restart.
//
// Registration order is preserved for listing so the admin console shows the
// built-in platforms in their familiar order and new ones after them.
type Registry struct {
	// mu guards the whole registry (providers, order, descriptions, hooks).
	// Holding ONE lock for all of it is deliberate: a registration publishes
	// the adapter and its description together, so no reader can observe a
	// new platform with an empty catalog entry. Mutations fire change hooks
	// only after unlocking, and hooks must not re-enter a mutation from
	// within the mutation's own goroutine (the gateway guards that with its
	// own gate).
	mu        sync.RWMutex
	providers map[string]Provider
	// order tracks registration order; removed names leave a gap that
	// Snapshot and Names skip.
	order  []string
	closed bool
	info   map[string]Info
	hooks  []func()
	// hidden holds the descriptions of names that are not registered: a
	// built-in declared before registration, or a platform whose accounts
	// still exist after it was removed. Register restores the description
	// from here, so re-registering a platform never reveals it with empty
	// metadata (a Descriptor's generic description would otherwise win).
	hidden map[string]Info
}

// Info is the admin-console description of a platform: everything the
// platform chooser needs to render a tile and create a valid account without
// any provider-specific code in the gateway. Providers may attach it to the
// registry with SetInfo (built-ins) or the admin API supplies it when an
// operator registers a platform at runtime.
type Info struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Tagline     string `json:"tagline"`
	Logo        string `json:"logo"`       // sprite symbol id; "" falls back to a letter tile
	IDPrefix    string `json:"id_prefix"`  // prefix for generated account ids
	BaseURL     string `json:"base_url"`   // default upstream origin
	Credential  string `json:"credential"` // credential field label
	Placeholder string `json:"placeholder"`
	Hint        string `json:"hint"` // one-line help under the credential field
	Auth        string `json:"auth"` // "api_key" or "oauth"
	// UIDField names an extra identity header the platform requires (e.g.
	// "X-User-Id"); empty means the credential alone authenticates. Realms
	// reports which X-WorkBuddy-Realm values the platform schedules in;
	// empty means the realm is irrelevant (defaults to "intl" in the tile).
	UIDField string   `json:"uid_field,omitempty"`
	Realms   []string `json:"realms,omitempty"`
	Quota    string   `json:"quota"` // "snapshot" or "unsupported"
	// Hot marks a platform registered at runtime through the admin API rather
	// than compiled in. Its adapter only covers the generic wire protocol.
	Hot bool `json:"hot,omitempty"`
}

// NamePattern is the accepted shape of a provider name. It is also used for
// account ids, so a provider name never breaks the account id rules.
const namePatternError = "平台名只能包含字母、数字、点、下划线和短横线，长度 1–64"

func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}, info: map[string]Info{}, hidden: map[string]Info{}}
}

// ValidateName checks a provider name (or namespace) for registration.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("provider name must not be empty")
	}
	if len(name) > 64 {
		return errors.New(namePatternError)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return errors.New(namePatternError)
	}
	return nil
}

// Descriptor is an optional Provider capability: a compiled-in adapter
// describes itself for the admin console. Register attaches the description.
type Descriptor interface {
	PlatformInfo() Info
}

// Register registers p under name and attaches its self description when the
// adapter carries one. This is the one call a built-in platform needs, so its
// adapter, display metadata and default endpoint stay in a single package.
func (r *Registry) Register(name string, p Provider) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if p == nil {
		return errors.New("provider implementation must not be nil")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("provider registry is closed")
	}
	if _, exists := r.providers[name]; exists {
		r.mu.Unlock()
		return fmt.Errorf("provider %q is already registered", name)
	}
	r.providers[name] = p
	listed := false
	for _, n := range r.order {
		if n == name {
			listed = true
			break
		}
	}
	if !listed {
		r.order = append(r.order, name)
	}
	if hidden, ok := r.hidden[name]; ok {
		r.info[name] = hidden
		delete(r.hidden, name)
	} else if d, ok := p.(Descriptor); ok {
		r.info[name] = d.PlatformInfo()
	}
	r.mu.Unlock()
	r.notify()
	return nil
}

// Get returns the provider registered under name, if any.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Has reports whether a provider is currently registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.providers[name]
	return ok
}

// Names returns the registered provider names in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for _, name := range r.order {
		if _, ok := r.providers[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

// Snapshot copies the name→provider map so a caller can iterate without
// holding the registry lock while talking to upstreams.
func (r *Registry) Snapshot() map[string]Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Provider, len(r.providers))
	for name, p := range r.providers {
		out[name] = p
	}
	return out
}

// Swap atomically replaces an existing registration (e.g. a hot-reloaded
// adapter build). An unknown name is an error.
func (r *Registry) Swap(name string, p Provider) error {
	if p == nil {
		return errors.New("provider implementation must not be nil")
	}
	r.mu.Lock()
	if _, exists := r.providers[name]; !exists {
		r.mu.Unlock()
		return fmt.Errorf("provider %q is not registered", name)
	}
	r.providers[name] = p
	r.mu.Unlock()
	r.notify()
	return nil
}

// Replace installs p under name whether or not the name exists: Remove then
// Register in one step. It never fails once the registry is open and p is
// non-nil, so "make this platform match this definition" is idempotent.
func (r *Registry) Replace(name string, p Provider) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if p == nil {
		return errors.New("provider implementation must not be nil")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("provider registry is closed")
	}
	_, exists := r.providers[name]
	listed := false
	for _, n := range r.order {
		if n == name {
			listed = true
			break
		}
	}
	r.providers[name] = p
	if !exists && !listed {
		r.order = append(r.order, name)
	}
	r.mu.Unlock()
	r.notify()
	return nil
}

// Remove unregisters a provider. It reports whether the name was registered.
// The description is kept aside rather than discarded: registering the same
// name again restores it.
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	if _, exists := r.providers[name]; !exists {
		r.mu.Unlock()
		return false
	}
	delete(r.providers, name)
	if info, ok := r.info[name]; ok {
		r.hidden[name] = info
		delete(r.info, name)
	}
	r.mu.Unlock()
	r.notify()
	return true
}

// Close stops further registration. In-process registries are never closed;
// only out-of-process adapter loaders close theirs so no request can be
// dispatched to an adapter whose process is being torn down.
func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

// SetInfo attaches the admin-console description of a platform. A host may
// declare it before the name is registered (the gateway replays the operator's
// saved platforms at boot this way); the description is held until then.
func (r *Registry) SetInfo(info Info) {
	r.mu.Lock()
	if _, ok := r.providers[info.ID]; ok {
		r.info[info.ID] = info
	} else {
		r.hidden[info.ID] = info
	}
	r.mu.Unlock()
}

// Info returns the description registered for name (zero value when absent).
func (r *Registry) Info(name string) (Info, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.info[name]
	return info, ok
}

// OnChange registers a callback invoked after every successful registry
// mutation (register, swap, replace, remove). The gateway uses it to reload
// account validation and re-probe accounts of a platform that just changed.
func (r *Registry) OnChange(fn func()) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.hooks = append(r.hooks, fn)
	r.mu.Unlock()
}

// notify runs change hooks without holding the registry lock, so a hook may
// read the registry or register further providers.
func (r *Registry) notify() {
	r.mu.RLock()
	hooks := append([]func(){}, r.hooks...)
	r.mu.RUnlock()
	for _, fn := range hooks {
		fn()
	}
}
