package provider

import (
	"errors"
	"fmt"
	"sort"
)

// Registry maps provider names to their implementations. It is the extension
// point for additional upstream platforms: build a registry, register your
// providers, and hand it to the gateway instead of relying on the built-in
// defaults. The registry is safe to share across gateway instances.
type Registry struct {
	providers map[string]Provider
}

func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

// Register adds a provider under a name. Names must be nonempty and unique;
// registering twice is an error so a misconfigured registry cannot silently
// shadow a provider.
func (r *Registry) Register(name string, p Provider) error {
	if name == "" {
		return errors.New("provider name must not be empty")
	}
	if _, exists := r.providers[name]; exists {
		return fmt.Errorf("provider %q is already registered", name)
	}
	r.providers[name] = p
	return nil
}

// Get returns the provider registered under name, if any.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// Names returns the registered provider names in stable sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
