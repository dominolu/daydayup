package gateway

// Hot platform registration (admin console → 平台):
//   - GET    /admin/v1/providers                 catalog of the live registry
//   - GET    /admin/v1/providers/{name}          one platform's catalog entry
//   - POST   /admin/v1/providers                 register a platform
//   - PUT    /admin/v1/providers/{name}          edit a platform description
//   - DELETE /admin/v1/providers/{name}          remove a platform
//
// A registration makes the platform routable immediately: the change hook
// publishes the registry's names to config (the account-validation gate),
// installs the default endpoint, reloads the effective account set and probes
// the platform's accounts, so no restart is needed either to add or to remove
// one. Operator registrations persist in the config file's
// provider_overrides section and are replayed at boot.
//
// The code a platform runs is whatever the live registry holds for its name:
// in-process that is provider.Generic (the OpenAI-compatible adapter), and a
// compiled-in or hot-reloadable adapter build can be substituted through
// Registry.Swap. Calls resolve the adapter when a request is dispatched, so a
// swap never changes a request that is already streaming.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"safuno/internal/config"
	"safuno/internal/pool"
	"safuno/internal/provider"
)

const providerAdminBase = "/admin/v1/providers"

var providerIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

// providerInput is the admin payload for registering or updating a platform.
// Pointer fields distinguish "absent" (keep) from "empty" (clear/derive).
type providerInput struct {
	ID          string    `json:"id"`
	Name        *string   `json:"name"`
	Tagline     *string   `json:"tagline"`
	Logo        *string   `json:"logo"`
	IDPrefix    *string   `json:"id_prefix"`
	BaseURL     *string   `json:"base_url"`
	Credential  *string   `json:"credential"`
	Placeholder *string   `json:"placeholder"`
	Hint        *string   `json:"hint"`
	Auth        *string   `json:"auth"`
	UIDField    *string   `json:"uid_field"`
	Realms      *[]string `json:"realms"`
	Quota       *string   `json:"quota"`
	// BuiltIn=true pins a platform to its compiled-in adapter (an update of a
	// built-in description); false (default) runs the generic adapter.
	BuiltIn *bool `json:"built_in"`
}

// providerAdmin serves the platform management endpoints; ok=false means the
// path does not belong to it.
func (s *Server) providerAdmin(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if path != providerAdminBase && !strings.HasPrefix(path, providerAdminBase+"/") {
		return false
	}
	name := strings.TrimPrefix(strings.TrimPrefix(path, providerAdminBase), "/")
	if strings.Contains(name, "/") {
		return false
	}
	switch {
	case name == "" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"data": s.platformCatalog(), "quota_sync": "external_snapshot", "official_quota_api": "unsupported"})
		return true
	case name == "" && r.Method == http.MethodPost:
		s.registerProvider(w, r)
		return true
	case name != "" && r.Method == http.MethodGet:
		if !s.providers.Has(name) {
			apiError(w, 404, "provider_not_found", "平台未注册")
			return true
		}
		writeJSON(w, 200, map[string]any{"data": s.platformEntry(name)})
		return true
	case name != "" && r.Method == http.MethodPut:
		s.updateProvider(w, r, name)
		return true
	case name != "" && r.Method == http.MethodDelete:
		s.removeProvider(w, name)
		return true
	}
	apiError(w, 405, "method_not_allowed", "unsupported method for platform management")
	return true
}

// applyInput overlays the present fields of in onto info and validates the
// result. in.ID is authoritative.
func applyInput(info provider.Info, in providerInput) (provider.Info, error) {
	if err := provider.ValidateName(in.ID); err != nil {
		return provider.Info{}, err
	}
	if !providerIDPattern.MatchString(in.ID) {
		return provider.Info{}, errors.New("平台名不合法")
	}
	info.ID = in.ID
	// Each present string field is trimmed and length-checked; nil keeps the
	// current value.
	type stringField struct {
		dst   *string
		src   *string
		limit int
		label string
	}
	fields := []stringField{
		{&info.Name, in.Name, 60, "平台显示名"},
		{&info.Tagline, in.Tagline, 120, "平台简介"},
		{&info.Logo, in.Logo, 64, "logo"},
		{&info.IDPrefix, in.IDPrefix, 24, "账号 ID 前缀"},
		{&info.Credential, in.Credential, 40, "凭证字段名"},
		{&info.Placeholder, in.Placeholder, 120, "凭证占位符"},
		{&info.Hint, in.Hint, 300, "凭证提示"},
		{&info.UIDField, in.UIDField, 64, "身份头"},
	}
	for _, f := range fields {
		if f.src == nil {
			continue
		}
		v := strings.TrimSpace(*f.src)
		if len([]rune(v)) > f.limit || strings.ContainsAny(v, "\r\n\x00") {
			return provider.Info{}, errors.New(f.label + "过长或包含非法字符")
		}
		*f.dst = v
	}
	if in.Auth != nil {
		switch strings.TrimSpace(*in.Auth) {
		case "", "api_key":
			info.Auth = "api_key"
		case "oauth":
			info.Auth = "oauth"
		default:
			return provider.Info{}, errors.New("认证方式必须是 api_key 或 oauth")
		}
	}
	if in.Quota != nil {
		switch strings.TrimSpace(*in.Quota) {
		case "":
			info.Quota = "unsupported"
		case "snapshot", "unsupported":
			info.Quota = strings.TrimSpace(*in.Quota)
		default:
			return provider.Info{}, errors.New("额度模式必须是 snapshot 或 unsupported")
		}
	}
	if in.Realms != nil {
		realms := []string{}
		for _, r := range *in.Realms {
			r = strings.TrimSpace(r)
			if r == "" {
				continue
			}
			if r != "intl" && r != "cn" {
				return provider.Info{}, errors.New("区域必须是 intl 或 cn")
			}
			realms = append(realms, r)
		}
		info.Realms = realms
	}
	if in.BaseURL != nil {
		raw := strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
		if raw != "" {
			if err := config.ValidateBaseURL(raw); err != nil {
				return provider.Info{}, err
			}
		}
		info.BaseURL = raw
	}
	if info.Name == "" {
		info.Name = info.ID
	}
	if info.IDPrefix == "" {
		// Generated account ids read like "nova-3f2a…"; the console uses this
		// prefix when creating an account for the platform.
		info.IDPrefix = strings.ToLower(info.ID)
		if len(info.IDPrefix) > 16 {
			info.IDPrefix = info.IDPrefix[:16]
		}
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
	return info, nil
}

func (s *Server) registerProvider(w http.ResponseWriter, r *http.Request) {
	var in providerInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.BuiltIn != nil && *in.BuiltIn {
		apiError(w, 400, "invalid_provider", "运行时注册的平台使用通用适配器，不能声明为 built_in")
		return
	}
	info, err := applyInput(provider.Info{Hot: true}, in)
	if err != nil {
		apiError(w, 400, "invalid_provider", err.Error())
		return
	}
	// Only the platform mutation holds providerMu; the change hooks fire while
	// this handler runs and must not re-enter it.
	s.providerMu.Lock()
	if s.providers.Has(info.ID) {
		s.providerMu.Unlock()
		apiError(w, 409, "provider_exists", "平台已注册；修改请使用 PUT /admin/v1/providers/{name}")
		return
	}
	// The platform is declared before it enters the registry, so the change
	// hook (account reload) already sees its endpoint.
	installDeclaration(info)
	if err := s.providers.Register(info.ID, provider.Generic{}); err != nil {
		dropDeclaration(info.ID)
		s.providerMu.Unlock()
		apiError(w, 400, "invalid_provider", err.Error())
		return
	}
	// Publish the operator description. It must follow Register, not precede
	// it: a plain registry registration (provider.Generic does not describe
	// itself) would otherwise claim the name slot.
	s.providers.SetInfo(info)
	if err := s.saveProviderOverrides(); err != nil {
		// Never advertise a registration that a restart would forget.
		s.providers.Remove(info.ID)
		dropDeclaration(info.ID)
		s.providerMu.Unlock()
		apiError(w, 500, "config_save_failed", "平台已回滚：描述未能写入配置文件")
		return
	}
	s.providerMu.Unlock()
	// The new platform is routable as soon as its accounts appear in the
	// account set. The change hook that fired inside the critical section only
	// marked it stale (it could not take providerMu re-entrantly), so commit
	// now, then discover models before responding so the console and
	// GET /v1/models agree immediately.
	s.refreshProviders()
	probeCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	for _, a := range s.Pool.Accounts() {
		if a.Provider == info.ID {
			s.checkAccount(probeCtx, a)
		}
	}
	writeJSON(w, 201, map[string]any{"data": s.platformEntry(info.ID)})
}

func (s *Server) updateProvider(w http.ResponseWriter, r *http.Request, name string) {
	previous, ok := s.providers.Info(name)
	if !ok || !s.providers.Has(name) {
		apiError(w, 404, "provider_not_found", "平台未注册")
		return
	}
	var in providerInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.ID != "" && in.ID != name {
		apiError(w, 400, "immutable_id", "平台名不可修改")
		return
	}
	in.ID = name
	next, err := applyInput(previous, in)
	if err != nil {
		apiError(w, 400, "invalid_provider", err.Error())
		return
	}
	s.providerMu.Lock()
	defer func() {
		s.providerMu.Unlock()
		s.refreshProviders()
	}()
	wantBuiltIn := previous.Hot == false
	if in.BuiltIn != nil {
		wantBuiltIn = *in.BuiltIn
	}
	next.Hot = !wantBuiltIn
	// Persist + activate: write first so a failed save leaves runtime and disk
	// unchanged, then re-install the declaration and reload accounts.
	prevSpec := config.Endpoint(name)
	prevRealms := config.RealmValues(name)
	installDeclaration(next)
	s.providers.SetInfo(next)
	if err := s.saveProviderOverrides(); err != nil {
		config.SetEndpoint(name, prevSpec)
		config.SetRealmPlatform(name, prevRealms, config.Products(name), nil)
		s.providers.SetInfo(previous)
		apiError(w, 500, "config_save_failed", "平台描述未能写入配置；运行中的描述保留")
		return
	}
	writeJSON(w, 200, map[string]any{"data": s.platformEntry(name)})
}

func (s *Server) removeProvider(w http.ResponseWriter, name string) {
	s.providerMu.Lock()
	defer func() {
		s.providerMu.Unlock()
		s.refreshProviders()
	}()
	if !s.providers.Has(name) {
		apiError(w, 404, "provider_not_found", "平台未注册")
		return
	}
	// A platform must not disappear mid-request: its accounts are idle by the
	// time it is removed (the same rule account edits follow).
	for _, a := range s.Pool.Accounts() {
		if a.Provider != name {
			continue
		}
		if state, ok := s.accountStates(a.ID).(pool.State); ok && state.InFlight > 0 {
			apiError(w, 409, "provider_busy", "该平台仍有进行中的请求，请稍后重试")
			return
		}
	}
	if !s.providers.Remove(name) {
		apiError(w, 404, "provider_not_found", "平台未注册")
		return
	}
	config.SetEndpoint(name, config.EndpointSpec{})
	config.SetRealmPlatform(name, nil, nil, nil)
	if err := s.saveProviderOverrides(); err != nil {
		// Restore both the adapter and its declaration so the removal can be
		// retried instead of leaving a half-removed platform.
		info := provider.Info{ID: name, Hot: true, Name: name, Auth: "api_key", Quota: "unsupported"}
		_ = s.providers.Register(name, provider.Generic{})
		installDeclaration(info)
		apiError(w, 500, "config_save_failed", "平台移除未能写入配置，已回滚")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "removed": name})
}

// installDeclaration publishes the endpoint of a platform, so account
// normalization and the console catalog agree with it.
//
// Realm vocabulary is only declared when the platform lists realms AND a
// default endpoint to derive them from: a realm declaration with an empty
// origin would make accounts of the platform fall back to the endpoint of a
// different realm (WorkBuddy's realmOf(a) is the account's cached realm).
func installDeclaration(info provider.Info) {
	spec := config.EndpointSpec{Origin: info.BaseURL}
	if info.BaseURL != "" && len(info.Realms) > 0 {
		spec.Realms = map[string]string{}
		for _, realm := range info.Realms {
			spec.Realms[realm] = info.BaseURL
		}
	}
	config.SetEndpoint(info.ID, spec)
	if info.BaseURL != "" && len(info.Realms) > 0 {
		config.SetRealmPlatform(info.ID, info.Realms, nil, nil)
	} else {
		config.SetRealmPlatform(info.ID, nil, nil, nil)
	}
}

func dropDeclaration(name string) {
	config.SetEndpoint(name, config.EndpointSpec{})
	config.SetRealmPlatform(name, nil, nil, nil)
}

// saveProviderOverrides writes every operator-registered platform into the
// config file's provider_overrides section. Built-in platform descriptions
// come from their adapters and are not persisted.
func (s *Server) saveProviderOverrides() error {
	overrides := make([]config.ProviderOverride, 0)
	for _, id := range s.providers.Names() {
		info, ok := s.providers.Info(id)
		if !ok || !info.Hot {
			continue
		}
		overrides = append(overrides, config.ProviderOverride{
			ID:          info.ID,
			Name:        info.Name,
			Tagline:     info.Tagline,
			Logo:        info.Logo,
			IDPrefix:    info.IDPrefix,
			BaseURL:     info.BaseURL,
			Credential:  info.Credential,
			Placeholder: info.Placeholder,
			Hint:        info.Hint,
			Auth:        info.Auth,
			UIDField:    info.UIDField,
			Realms:      info.Realms,
			Quota:       info.Quota,
		})
	}
	return s.writeConfigField("provider_overrides", overrides)
}

func (s *Server) platformEntry(name string) PlatformInfo {
	for _, p := range s.platformCatalog() {
		if p.ID == name {
			return p
		}
	}
	return PlatformInfo{ID: name}
}
