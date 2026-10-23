package gateway

import (
	"safuno/internal/config"
	"safuno/internal/provider"
)

// PlatformInfo is the admin-console description of a registered platform.
//
// The console renders the platform chooser from this catalog instead of a
// hard-coded list. The catalog is derived from the live provider registry, so
// it is always exactly the set of platforms the gateway can route to right
// now: built-in adapters (which describe themselves through
// provider.Descriptor) and platforms registered or removed at runtime through
// the admin API both appear with no front-end or gateway change.
type PlatformInfo = provider.Info

// platformCatalog returns the console catalog for every registered provider,
// in registry (registration) order so tiles stay stable.
func (s *Server) platformCatalog() []PlatformInfo {
	if s.providers == nil {
		return nil
	}
	names := s.providers.Names()
	out := make([]PlatformInfo, 0, len(names))
	for _, id := range names {
		info, ok := s.providers.Info(id)
		if !ok {
			info = PlatformInfo{Auth: "api_key", Quota: "unsupported"}
		}
		info.ID = id
		if info.Name == "" {
			info.Name = id
		}
		if info.Credential == "" {
			info.Credential = "API Key"
		}
		if info.Auth == "" {
			info.Auth = "api_key"
		}
		if info.Quota == "" {
			info.Quota = "unsupported"
		}
		if info.BaseURL == "" {
			info.BaseURL = config.DefaultBaseURL(id, "")
		}
		out = append(out, info)
	}
	return out
}
