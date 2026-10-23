package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"safuno/internal/accounts"
	"safuno/internal/agents"
	"safuno/internal/config"
	"safuno/internal/pool"
	"safuno/internal/provider"
	"safuno/internal/provider/workbuddy"
	"safuno/internal/redact"
	"safuno/internal/sessions"
)

type Server struct {
	Sessions               *sessions.Store
	Config                 config.Config
	Pool                   *pool.Pool
	providers              *provider.Registry
	// providerMu serializes platform mutations (admin add/edit/remove and the
	// boot replay). Ordinary provider lookups never take it.
	providerMu sync.Mutex
	// providersDirty is set by the registry change hooks and cleared once the
	// account set and default platform have been re-derived from the registry.
	providersDirty atomic.Bool
	client                 *http.Client
	refreshGate            chan struct{}
	accountsMu             sync.Mutex
	accountStore           *accounts.Store
	agentStore             *agents.Store
	agentSelfCheckOverride string
	routeLog               *routeLog
	waitActive             atomic.Int64
	versions               *versionCache
	agentsModeMu           sync.RWMutex
	agentsMode             string // default write mode, switchable from the UI without restart
	logger                 *slog.Logger
	waitSlots              chan struct{}
	redact                 *redact.Engine
	redactErr              error
	dbErr                  error // persistence (database or migration) failure at startup
	redactionMu            sync.RWMutex
	redactionSpans         *redactionSpanLog
	// wbLogins tracks in-flight WorkBuddy browser OAuth login states. Only the
	// state string, realm and start time are kept; credentials only appear
	// once poll_login completes and the account is persisted.
	wbLoginMu  sync.Mutex
	wbLogins   map[string]wbLoginRecord
	wbLoginIPs map[string]wbLoginStamp
	// adminSession is the per-process secret behind the console's automatic
	// same-origin session. GET /admin/ hands the browser this HttpOnly value,
	// and the admin API accepts it alongside the Bearer GATEWAY_ADMIN_KEY that
	// scripts and remote clients keep using. No credential prompt is shown.
	adminSession string
}

func New(c config.Config) *Server {
	reg := provider.NewRegistry()
	installBuiltins(reg)
	return NewWithProviders(c, reg)
}

// NewWithProviders builds the gateway with an explicit provider registry.
// The registry is the extension boundary: register additional upstream
// platforms here instead of modifying the gateway itself. Platform
// descriptions (tiles, default endpoints, realm/product vocabulary) travel
// with the adapter through provider.Descriptor, so a new compiled-in platform
// needs no gateway change.
func NewWithProviders(c config.Config, providers *provider.Registry) *Server {
	installBuiltins(providers)
	// Operator-registered platforms from a previous run are available before
	// any account is validated or routed; built-ins win a name clash.
	applyProviderOverrides(providers, c.ProviderOverrides)
	// Account validation accepts exactly what the registry holds, and it
	// follows registry changes made later by the admin console.
	syncRegisteredProviders(providers)
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.ResponseHeaderTimeout = 90 * time.Second
	t.TLSHandshakeTimeout = 10 * time.Second
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 20
	// Compression is transparent; identity preserves byte-for-byte observation.
	t.DisableCompression = true
	// Legacy storage artifacts migrate into the shared database first.
	dbErr := migrateLegacyData(c)
	accountStore, accountErr := accounts.New(c.DBFile)
	if accountErr != nil && dbErr == nil {
		dbErr = accountErr
	}
	s := &Server{
		Config:       c,
		Pool:         pool.New(c.Accounts),
		accountStore: accountStore,
		providers:    providers,
		refreshGate:  make(chan struct{}, 1),
		routeLog:     newRouteLog(512),
		versions:     newVersionCache(),
		agentsMode:   defaultWriteMode(c.Agents.WriteMode),
		wbLogins:     make(map[string]wbLoginRecord),
		wbLoginIPs:   make(map[string]wbLoginStamp),
		dbErr:        dbErr,
		client: &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
	s.configurePool()
	// A platform added or removed while the gateway runs: reload the effective
	// account set, drop persisted defaults naming a removed platform, and
	// re-probe accounts whose adapter was just installed or swapped.
	providers.OnChange(s.providerChanged)
	s.adminSession = newAdminSession()
	if c.Redaction != nil && c.Redaction.Enabled {
		cfg := *c.Redaction
		// The redaction reverse index lives in the shared database now.
		cfg.StoreFile = c.DBFile
		if err := cfg.Normalize(); err != nil {
			s.redactErr = err
		} else if engine, err := redact.New(cfg); err != nil {
			s.redactErr = err
		} else {
			s.redact = engine
			s.redact.SetSpanHook(s.logRedactionSpan)
			s.redactionSpans = newRedactionSpanLog(2000)
			s.Config.Redaction = &cfg
		}
	}
	s.logger = slog.Default()
	waitLimit := c.MaxWaitingRequests
	if waitLimit <= 0 {
		waitLimit = 128
	}
	s.waitSlots = make(chan struct{}, waitLimit)
	return s
}

// newAdminSession returns the random, restart-scoped token the console uses
// as an automatic same-origin credential. An empty value (the only failure
// mode of crypto/rand) simply disables auto-login and leaves the Bearer admin
// key as the sole admin credential.
func newAdminSession() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// builtinProviders is the compiled-in platform set. A host embedding the
// gateway may add its own adapters through NewWithProviders.
func builtinProviders() map[string]provider.Provider {
	return map[string]provider.Provider{
		"sensenova": provider.SenseNova{},
		"workbuddy": workbuddy.WorkBuddy{},
		"tokenplan": provider.TokenPlan{},
	}
}

// BuiltinProviderNames lists the compiled-in platform names in catalog order.
func BuiltinProviderNames() []string {
	reg := provider.NewRegistry()
	installBuiltins(reg)
	return reg.Names()
}

// installBuiltins registers the built-ins that the given registry lacks, in a
// stable order. Adapters describe themselves (provider.Descriptor), so no
// display table lives here.
func installBuiltins(reg *provider.Registry) {
	for _, name := range []string{"sensenova", "workbuddy", "tokenplan"} {
		if reg.Has(name) {
			continue
		}
		if p, ok := builtinProviders()[name]; ok {
			_ = reg.Register(name, p)
		}
	}
}

// applyProviderOverrides replays operator-registered platforms onto the
// registry and declares their endpoints and realm vocabulary. A name that is
// already taken (a built-in, or an adapter a host installed) is left alone.
func applyProviderOverrides(reg *provider.Registry, overrides []config.ProviderOverride) {
	for _, o := range overrides {
		if o.ID == "" || reg.Has(o.ID) {
			continue
		}
		if provider.ValidateName(o.ID) != nil {
			continue
		}
		if err := reg.Register(o.ID, provider.Generic{}); err != nil {
			continue
		}
		info := provider.Info{
			ID:          o.ID,
			Name:        o.Name,
			Tagline:     o.Tagline,
			Logo:        o.Logo,
			IDPrefix:    o.IDPrefix,
			BaseURL:     o.BaseURL,
			Credential:  o.Credential,
			Placeholder: o.Placeholder,
			Hint:        o.Hint,
			Auth:        o.Auth,
			UIDField:    o.UIDField,
			Realms:      o.Realms,
			Quota:       o.Quota,
			Hot:         true,
		}
		if info.Name == "" {
			info.Name = o.ID
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
		reg.SetInfo(info)
		installDeclaration(info)
	}
}

// providerChanged runs after any registry mutation (boot, admin registration,
// adapter swap): it republishes the platform names to config — the gate
// account validation consults — and marks the running state as stale.
//
// It must never recover that state itself. Registry mutations call their
// hooks synchronously, ordinarily on a request goroutine that already holds
// providerMu (the admin page calling itself) or in the middle of
// NewWithProviders; reloading accounts from inside the hook would either
// deadlock or, when the mutation came from a helper goroutine, race the admin
// transaction that has not even registered its platform yet.
func (s *Server) providerChanged() {
	syncRegisteredProviders(s.providers)
	s.markProvidersStale()
}

// markProvidersStale flags the effective account set for re-derivation. The
// next platform operation (the end of every admin mutation, or the boot
// path) calls refreshProviders once it has committed.
func (s *Server) markProvidersStale() {
	s.providersDirty.Store(true)
}

// refreshProviders recovers the running state from the registry after a
// platform change: the effective account set (a managed account of a removed
// platform is no longer routed, because the account store refuses accounts
// whose platform is not registered) and the default platform.
//
// It runs under providerMu and leaves the dirty flag set when the lock is
// contended instead of blocking: the holder is the mutation that owns the
// change and calls it again before returning.
func (s *Server) refreshProviders() {
	if !s.providerMu.TryLock() {
		return
	}
	defer s.providerMu.Unlock()
	s.refreshProvidersLocked()
}

func (s *Server) refreshProvidersLocked() {
	s.providersDirty.Store(false)
	// A managed account of a removed platform disappears from routing because
	// the account store refuses accounts whose platform is not registered.
	if s.accountStore == nil {
		return
	}
	if err := s.LoadAccounts(); err != nil {
		slog.Warn("account reload after provider change failed", "error", err)
	}
	if !s.providers.Has(s.Config.DefaultProvider) {
		s.Config.DefaultProvider = "sensenova"
		if err := s.writeConfigField("default_provider", s.Config.DefaultProvider); err != nil {
			slog.Warn("default_provider fallback could not be persisted", "error", err)
		}
	}
}

// syncRegisteredProviders publishes the registry's names to config, the gate
// account validation consults.
func syncRegisteredProviders(reg *provider.Registry) {
	config.SetRegisteredProviders(reg.Names())
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.serveAdminUI(w, r) {
		return
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	trace := &requestTrace{ID: hex.EncodeToString(id), Started: time.Now()}
	w.Header().Set("X-Request-ID", trace.ID)
	r = withTrace(r, trace)
	logged := &logResponseWriter{ResponseWriter: w, trace: trace}
	w = logged
	defer func() {
		if recovered := recover(); recovered != nil {
			trace.Incomplete = true
			if trace.Failure.Reason == "" {
				trace.Failure = diagnostic{Layer: "gateway", Reason: "handler_aborted", Message: "request handler aborted"}
			}
			s.logCompletion(r, logged)
			panic(recovered)
		}
		s.logCompletion(r, logged)
	}()
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/") || r.URL.Path == "/metrics" {
		if !s.adminAuthorized(r) {
			apiError(w, 401, "unauthorized", "admin credential required")
			return
		}
		s.admin(w, r)
		return
	}
	if !authorized(r, s.Config.GatewayKey) {
		apiError(w, 401, "unauthorized", "gateway credential required")
		return
	}
	if s.toolReport(w, r) {
		return
	}
	// Provider selection is an explicit filter, never a routing domain: an
	// empty provider means the request is routed across the aggregated
	// model→account mapping of every registered platform (any account whose
	// account supports the requested model is a candidate). X-Provider or a
	// /providers/{provider}/… path still pin a single platform for clients
	// that need a specific identity.
	prov := r.Header.Get("X-Provider")
	path := r.URL.Path
	if strings.HasPrefix(path, "/providers/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/providers/"), "/", 2)
		if len(parts) != 2 {
			apiError(w, 404, "not_found", "expected /providers/{provider}/v1/...")
			return
		}
		prov = parts[0]
		path = "/" + parts[1]
	}
	// Group pinning has a URL form too: /groups/{group}/v1/... is the stable
	// per-group Base URL the admin console advertises, equivalent to sending
	// the X-Group header. It takes precedence over the header when both are
	// present; an invalid name in the path is rejected outright.
	pathGroup := ""
	if strings.HasPrefix(path, "/groups/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/groups/"), "/", 2)
		if len(parts) != 2 {
			apiError(w, 404, "not_found", "expected /groups/{group}/v1/...")
			return
		}
		var err error
		if pathGroup, err = config.NormalizeGroup(parts[0]); err != nil {
			apiError(w, 400, "invalid_group", err.Error())
			return
		}
		path = "/" + parts[1]
	}
	// The empty provider is the global routing mode, not an unknown platform;
	// an explicitly selected platform must be a registered adapter.
	if prov != "" {
		if _, ok := s.providers.Get(prov); !ok {
			trace.Provider = prov
			apiError(w, 400, "unknown_provider", "provider is not registered")
			return
		}
	}
	trace.Provider = prov
	// The routing group bounds the aggregated model→account pool: requests
	// under group G only ever see accounts of group G (across platforms).
	// /groups/{group}/v1/... or the X-Group header selects a group; absent,
	// the configured default_group applies.
	group := pathGroup
	if group == "" {
		group = r.Header.Get("X-Group")
		if group != "" {
			var err error
			if group, err = config.NormalizeGroup(group); err != nil {
				apiError(w, 400, "invalid_group", err.Error())
				return
			}
		}
	}
	if group == "" {
		group = s.Config.DefaultGroup
		if group == "" {
			group = pool.DefaultGroup
		}
	}
	trace.Group = group
	if realm := r.Header.Get("X-WorkBuddy-Realm"); realm != "" && realm != "intl" && realm != "cn" {
		apiError(w, 400, "invalid_realm", "X-WorkBuddy-Realm must be intl or cn")
		return
	} else if realm != "" {
		trace.Realm = realm
	} else {
		trace.Realm = s.Config.DefaultRealm
	}
	if !strings.HasPrefix(path, "/v1/") || !validPath(path) || r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" {
		apiError(w, 400, "unsupported_request", "expected an HTTP request under /v1/ without path traversal or upgrade")
		return
	}
	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, s.Config.MaxRequestBytes))
	if s.redactErr != nil {
		apiError(w, 503, "redaction_unavailable", "redaction engine is unavailable")
		return
	}
	if s.redact != nil && readErr == nil {
		// Mask before session archival and forwarding: what leaves the gateway
		// (and is archived) never contains the original sensitive spans.
		if maskableContentType(r.Header.Get("Content-Type")) {
			masked, spans, err := s.maskRequestBody(r, body)
			if err != nil {
				var inputError *redactionInputError
				if errors.As(err, &inputError) {
					apiError(w, inputError.status, "invalid_body", inputError.message)
					return
				}
				s.redactRecordError(err)
				if s.redactFailClosed() {
					slog.Warn("redaction masking failed; request rejected", "error", err, "request_id", trace.ID)
					apiError(w, 500, "redaction_failed", "sensitive content could not be masked")
					return
				}
				slog.Warn("redaction masking failed; forwarding unmasked", "error", err, "request_id", trace.ID)
			} else {
				body = masked
				trace.MaskedSpans = spans
			}
		}
	}
	trace.Model = modelFrom(r, body)
	if !s.beginSession(w, r, prov, path, body, readErr == nil) {
		return
	}
	if readErr != nil {
		var large *http.MaxBytesError
		if errors.As(readErr, &large) {
			apiError(w, 413, "request_too_large", "request exceeds max_request_bytes")
		} else {
			apiError(w, 400, "body_read_failed", "could not read request body")
		}
		return
	}
	if path == "/v1/models" && r.Method == http.MethodGet {
		ttl := 3 * time.Duration(s.Config.RefreshIntervalSeconds) * time.Second
		var models []provider.Model
		var ok bool
		if prov == "" {
			models, ok = s.Pool.ModelListGroup(group, ttl)
		} else {
			models, ok = s.Pool.ModelList(prov, group, ttl)
		}
		if !ok {
			s.Refresh(r.Context())
			if prov == "" {
				models, ok = s.Pool.ModelListGroup(group, ttl)
			} else {
				models, ok = s.Pool.ModelList(prov, group, ttl)
			}
		}
		if !ok {
			apiError(w, 503, "models_unavailable", "no fresh upstream model discovery is available")
			return
		}
		if s.Config.Intent.Enabled {
			models = append(append([]provider.Model{}, models...), map[string]any{"id": "intent", "name": "Intent Routing（按需在省钱/强模型间分类）", "intent": true})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": models})
		return
	}
	// The client's stream preference (the bridge/middleware writers follow it,
	// not the bridge flag which is derived from the same body for bridges).
	requestStream := bodyStream(body)
	// Protocol bridge (M2): /v1/messages and /v1/responses are translated to
	// Chat Completions before forwarding, and the Chat response is translated
	// back while streaming. On conversion error maybeBridge already answered.
	bridged, handled := s.maybeBridge(w, path, body)
	if handled && bridged.kind == bridgeNone {
		return
	}
	if bridged.kind != bridgeNone {
		trace.BridgeKind = bridged.kind
		trace.BridgeStream = bodyStream(bridged.body)
		body = bridged.body
		path = bridgeClientPath(path)
	}
	// M4 intent routing classifies the new turn on the ORIGINAL client model
	// first, then the middleware (model_map / system_prompt) rewrites the final
	// body (so the chosen simple/strong model also passes through model_map).
	if s.Config.Intent.Enabled {
		if next, routed := s.applyIntent(r, trace, body); routed {
			body = next
		}
	}
	body = s.applyMiddlewareBody(trace, body)
	var bw *bridgeWriter
	if trace.BridgeKind != bridgeNone {
		bw = newBridgeWriter(w, trace.BridgeKind, trace.BridgeStream)
		w = bw
	}
	// M4 response post-processor (model rename / think_tags strip) sits between
	// the proxy and the protocol bridge so it applies to every client protocol.
	var mw *chatRewriteWriter
	if s.middlewareResponseActive() {
		mw = newChatRewriteWriter(w, requestStream, s.middlewareShouldStripReasoning(), func(model string) string {
			// model_rename renames the answering platform's upstream id back to
			// the canonical client name; middleware.model_map aliases are the
			// fallback (and handle every other response model).
			if tr := traceFrom(r); tr != nil && tr.UpstreamModel != "" && tr.UpstreamModel != tr.Model && model == tr.UpstreamModel {
				return tr.Model
			}
			return s.middlewareClientFor(model)
		})
		w = mw
	}
	s.proxy(w, r, prov, path, body)
	if mw != nil && !requestStream {
		if err := mw.finalize(); err != nil {
			slog.Warn("middleware finalize failed", "error", err, "request_id", trace.ID)
		}
	}
	if bw != nil {
		if !trace.BridgeStream {
			if err := bw.finalize(); err != nil {
				slog.Warn("bridge finalize failed", "error", err, "request_id", trace.ID)
			}
		} else {
			bw.end()
		}
	}
}

// bodyStream reports whether a request body asks for streaming.
func bodyStream(body []byte) bool {
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if json.Unmarshal(body, &probe) != nil || probe.Stream == nil {
		return false
	}
	return *probe.Stream
}

func (s *Server) Refresh(ctx context.Context) {
	select {
	case s.refreshGate <- struct{}{}:
		defer func() { <-s.refreshGate }()
	case <-ctx.Done():
		return
	}
	for _, a := range s.Pool.Accounts() {
		if ctx.Err() != nil {
			return
		}
		s.checkAccount(ctx, a)
	}
}
func (s *Server) checkAccount(ctx context.Context, a config.Account) {
	p, ok := s.providers.Get(a.Provider)
	if !ok {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	models, err := p.Models(probeCtx, s.client, a)
	cancel()
	var he *provider.HTTPError
	var delay time.Duration
	if errors.As(err, &he) {
		delay = cooldown(he.Status, he.RetryAfter)
	}
	s.Pool.DiscoveryFor(a, models, err, delay)
	// WorkBuddy reports its credit balance through the billing API (no feed
	// URL needed). Refresh it at most every 10 minutes so the background
	// discovery cadence does not hammer the billing endpoint.
	refreshQuota := a.QuotaFeedURL != ""
	if a.Provider == "workbuddy" {
		refreshQuota = s.Pool.QuotaRefreshDue(a.ID, 2*time.Minute, 10*time.Minute)
	}
	if refreshQuota {
		quotaCtx, quotaCancel := context.WithTimeout(ctx, 10*time.Second)
		q, qerr := p.Quota(quotaCtx, s.client, a)
		quotaCancel()
		if qerr == nil {
			qerr = s.Pool.SetQuota(a.ID, &q)
		}
		s.Pool.QuotaError(a.ID, qerr)
	}
}

// sessionMaintenance runs the session archive maintenance passes once shortly
// after boot: BLOB→file migration, file/index reconciliation and retention.
// Each is chunked so the shared database connection is never held for long.
func (s *Server) sessionMaintenance(ctx context.Context) {
	if s.Sessions == nil {
		return
	}
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return
	}
	if s.Sessions.BodyFiles() {
		if err := s.Sessions.MigrateBodiesToFiles(); err != nil {
			slog.Error("session bodies migration failed", "error", err)
		}
		added, removed, err := s.Sessions.ReconcileFiles()
		if err != nil {
			slog.Error("session file reconciliation failed", "error", err)
		} else if added > 0 || removed > 0 {
			slog.Info("session file index reconciled", "indexed", added, "removed_dead", removed)
		}
	}
	if s.Config.Sessions.ShouldPrune() {
		purged, freed, err := s.Sessions.Prune()
		if err != nil {
			slog.Error("session retention failed", "error", err)
		} else if purged > 0 {
			slog.Info("session retention pruned", "body_rows", purged, "freed_mb", freed>>20)
		}
	}
}

// StoreError reports a startup database/migration failure, checked by main
// before any store is used.
func (s *Server) StoreError() error { return s.dbErr }

// Run performs discovery and checkpoints state for one process. Use one instance
// per state file; distributed quota accounting requires a shared transactional store.
func (s *Server) Run(ctx context.Context) {
	refresh := time.NewTicker(time.Duration(s.Config.RefreshIntervalSeconds) * time.Second)
	defer refresh.Stop()
	tokenRefresh := time.NewTicker(5 * time.Minute)
	defer tokenRefresh.Stop()
	save := time.NewTicker(5 * time.Second)
	defer save.Stop()
	retention := time.NewTicker(1 * time.Hour)
	defer retention.Stop()
	// Discovery must not hold up persistence or shutdown.
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	probeDone := make(chan struct{}, 1)
	start := func() { go func() { s.Refresh(probeCtx); probeDone <- struct{}{} }() }
	busy := true
	start()
	// Refresh any already-expiring OAuth tokens right after boot.
	s.refreshWorkBuddyLoop(probeCtx, 1*time.Hour)
	// Daily WorkBuddy bonus tasks (CN check-in / active report, intl daily
	// chat) at 09:15 / 21:15 local; per-account date guards prevent dupes.
	go s.workBuddyTasksLoop(probeCtx)
	// M4: daily quota-window pre-warm at the reset hour.
	go s.prewarmLoop(probeCtx)
	// Session archive maintenance: migrate legacy body BLOBs to per-session
	// files, reconcile the file index after any crash, then apply retention.
	// A large first cut takes tens of seconds, so it runs in the background
	// after boot instead of delaying startup.
	go s.sessionMaintenance(probeCtx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-probeDone:
			busy = false
		case <-refresh.C:
			if !busy {
				busy = true
				start()
			}
		case <-tokenRefresh.C:
			// OAuth access tokens are short-lived; rotate the accounts whose
			// JWT expiry is inside the proactive window.
			s.refreshWorkBuddyLoop(probeCtx, 1*time.Hour)
		case <-save.C:
			if err := s.FlushRedaction(); err != nil {
				slog.Error("redaction checkpoint failed", "error", err)
			}
			if err := s.Pool.Save(s.Config.DBFile); err != nil {
				slog.Error("state checkpoint failed", "error", err)
			}
			// Drain the WAL after every persistence tick so the next boot
			// never pays a blocking checkpoint-on-close of the multi-gigabyte
			// database (the dominant historical startup cost, ~16s).
			if err := s.Pool.Checkpoint(s.Config.DBFile); err != nil {
				slog.Debug("wal checkpoint failed", "error", err)
			}
		case <-retention.C:
			if s.Sessions == nil || !s.Config.Sessions.ShouldPrune() {
				continue
			}
			purged, freed, err := s.Sessions.Prune()
			if err != nil {
				slog.Error("session retention failed", "error", err)
			} else if purged > 0 {
				slog.Info("session retention pruned", "body_rows", purged, "freed_mb", freed>>20)
			}
		}
	}
}
