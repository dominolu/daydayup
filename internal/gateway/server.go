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

	"daydayup/internal/accounts"
	"daydayup/internal/agents"
	"daydayup/internal/config"
	"daydayup/internal/pool"
	"daydayup/internal/provider"
	"daydayup/internal/provider/workbuddy"
	"daydayup/internal/redact"
	"daydayup/internal/sessions"
)

type Server struct {
	Sessions               *sessions.Store
	Config                 config.Config
	Pool                   *pool.Pool
	providers              *provider.Registry
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
}

func New(c config.Config) *Server {
	reg := provider.NewRegistry()
	_ = reg.Register("sensenova", provider.SenseNova{})
	_ = reg.Register("workbuddy", workbuddy.WorkBuddy{})
	_ = reg.Register("tokenplan", provider.TokenPlan{})
	return NewWithProviders(c, reg)
}

// NewWithProviders builds the gateway with an explicit provider registry.
// The registry is the extension boundary: register additional upstream
// platforms here instead of modifying the gateway itself.
func NewWithProviders(c config.Config, providers *provider.Registry) *Server {
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
	s.Pool.SetGuards(c.Guards)
	s.Pool.SetRouting(c.Routing)
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

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if serveAdminUI(w, r) {
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
		if !authorized(r, s.Config.AdminKey) {
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
	// X-Group selects a group; absent, the configured default_group applies.
	group := r.Header.Get("X-Group")
	if group == "" {
		group = s.Config.DefaultGroup
		if group == "" {
			group = pool.DefaultGroup
		}
	} else {
		var err error
		if group, err = config.NormalizeGroup(group); err != nil {
			apiError(w, 400, "invalid_group", err.Error())
			return
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
		}
	}
}
