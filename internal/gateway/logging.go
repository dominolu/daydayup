package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"daydayup/internal/observe"
	"daydayup/internal/pool"
	"daydayup/internal/sessions"
)

type traceKey struct{}

// diagnostic is the gateway's view of an observe.Diagnostic; the response
// observation layer reports the same shape.
type diagnostic = observe.Diagnostic

type requestTrace struct {
	Purpose                                    string
	Title                                      *sessions.Title
	GeneratedTitle, TitleError                 string
	Identity                                   sessions.Identity
	ToolCallIDs                                []string
	Timing                                     *sessions.Timing
	UsageFields                                []string
	RequestOutputLimits                        map[string]int64
	RateLimitHeaders                           map[string][]string
	RateLimitHeadersTruncated                  bool
	SessionID, ResponseID, PinnedAccount       string
	RefreshRetryID                             string
	RefreshAttempted                           bool
	Capture                                    *sessions.Capture
	Stateful, Incomplete                       bool
	StreamTerminalReceived                     bool
	StreamTerminalForwarded                    bool
	MaskedSpans                                int
	BridgeKind                                 bridgeKind
	BridgeStream                               bool
	ID, Provider, Model, AccountID, UpstreamID string
	// UpstreamModel is the id the answering account was actually sent when
	// model_rename rewrites it; empty means it equals Model.
	UpstreamModel string
	Group                                      string
	Realm                                      string
	Started                                    time.Time
	Attempts                                   int
	PoolWaitMS                                 int64
	PoolWaits                                  int
	Usage                                      pool.Usage
	Failure                                    diagnostic
	credentials                                []string
	rejections                                 []pool.Rejection
	hops                                       []routeHop
}

func traceFrom(r *http.Request) *requestTrace {
	t, _ := r.Context().Value(traceKey{}).(*requestTrace)
	return t
}

// traceFromCtx is traceFrom for the raw context handed to library callbacks
// (for example redaction span hooks) that never see the request object.
func traceFromCtx(ctx context.Context) *requestTrace {
	t, _ := ctx.Value(traceKey{}).(*requestTrace)
	return t
}

type logResponseWriter struct {
	http.ResponseWriter
	trace  *requestTrace
	status int
	bytes  int64
}

func (w *logResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *logResponseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *logResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	if w.trace.Capture != nil && n > 0 {
		w.trace.Capture.WriteResponse(b[:n])
	}
	return n, err
}

func markGatewayError(w http.ResponseWriter, status int, code, message string) {
	layer := "gateway"
	if status == http.StatusUnauthorized {
		layer = "gateway_auth"
	}
	failure := diagnostic{Layer: layer, Reason: code, Code: code, Message: message}
	// Error responses travel through layered writers (protocol bridge, model
	// rename / think_tags rewrite), so the trace holder may not be the outer
	// writer. Walk the chain before giving up, otherwise the archived failure
	// keeps the last upstream attempt's label.
	for i := 0; i < 8 && w != nil; i++ {
		if lw, ok := w.(*logResponseWriter); ok {
			lw.trace.Failure = failure
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}

// Only protocol error identifiers are retained. Arbitrary error messages may
// echo prompts or credentials, so messages are limited to known safe summaries.
func protocolError(body []byte) diagnostic { return observe.ProtocolError(body) }

func upstreamDiagnostic(status int, d diagnostic) diagnostic {
	d.Layer = "upstream"
	if d.Reason != "" {
		return d
	}
	switch {
	case status == 401:
		d.Reason, d.Message = "upstream_auth_failed", "upstream rejected its account credential"
	case status == 403:
		d.Reason, d.Message = "upstream_permission_denied", "upstream denied permission"
	case status == 429:
		d.Reason, d.Message = "upstream_rate_or_quota_limit", "upstream rate or quota limit"
	case status >= 500:
		d.Reason, d.Message = "upstream_server_error", "upstream server error"
	case status >= 400:
		d.Reason, d.Message = "upstream_request_rejected", "upstream rejected the request"
	default:
		d.Reason, d.Message = "upstream_stream_error", "upstream reported an error inside the response"
	}
	return d
}

var credentialPattern = regexp.MustCompile(`(?i)\b(?:sk-[a-z0-9_-]+|bearer[\t ]+[a-z0-9_./+=-]+)`)

func (s *Server) logSanitizer(extra ...string) func(string) string {
	keys := []string{s.Config.GatewayKey, s.Config.AdminKey}
	keys = append(keys, extra...)
	for _, a := range s.Pool.Accounts() {
		keys = append(keys, a.APIKey, a.QuotaFeedKey)
	}
	return func(value string) string {
		for _, key := range keys {
			if key != "" {
				value = strings.ReplaceAll(value, key, "[REDACTED]")
			}
		}
		value = credentialPattern.ReplaceAllString(value, "[REDACTED]")
		value = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, value)
		runes := []rune(value)
		if len(runes) > 256 {
			return string(runes[:256]) + "…"
		}
		return value
	}
}

func (s *Server) logRecord(r *http.Request, event string, status int, elapsed time.Duration, failure diagnostic, extra ...any) {
	t := traceFrom(r)
	if t == nil {
		return
	}
	clean := s.logSanitizer(t.credentials...)
	attrs := []any{"event", event, "request_id", t.ID, "method", r.Method, "path", clean(r.URL.Path), "provider", clean(t.Provider), "group", clean(t.Group), "model", clean(t.Model), "account_id", clean(t.AccountID), "attempts", t.Attempts, "status", status, "duration_ms", elapsed.Milliseconds()}
	if t.UpstreamModel != "" && t.UpstreamModel != t.Model {
		attrs = append(attrs, "upstream_model", clean(t.UpstreamModel))
	}
	if t.PoolWaits > 0 {
		attrs = append(attrs, "pool_wait_ms", t.PoolWaitMS, "pool_waits", t.PoolWaits)
	}
	if t.Timing != nil && t.Timing.Streaming {
		attrs = append(attrs, "sse_terminal_received", t.StreamTerminalReceived, "sse_terminal_forwarded", t.StreamTerminalForwarded)
	}
	if t.Purpose != "" {
		attrs = append(attrs, "request_purpose", t.Purpose)
	}
	if t.Title != nil {
		attrs = append(attrs, "title_detector", t.Title.Detector, "title_association", t.Title.Association, "title_status", t.Title.Status)
	}
	if t.SessionID != "" {
		attrs = append(attrs, "session_id", clean(t.SessionID))
		attrs = append(attrs, "session_source", t.Identity.Source, "session_client", t.Identity.Client)
		if t.Identity.LinkedRequestID != "" {
			attrs = append(attrs, "linked_request_id", t.Identity.LinkedRequestID)
		}
	}
	if t.UpstreamID != "" {
		attrs = append(attrs, "upstream_request_id", clean(t.UpstreamID))
	}
	if len(t.rejections) > 0 {
		rejections := make([]map[string]string, 0, len(t.rejections))
		for _, rejection := range t.rejections {
			rejections = append(rejections, map[string]string{"account_id": clean(rejection.AccountID), "reason": rejection.Reason})
		}
		attrs = append(attrs, "pool_rejections", rejections)
	}
	level, outcome := slog.LevelInfo, "success"
	if failure.Reason != "" || status >= 400 {
		level, outcome = slog.LevelWarn, "error"
		attrs = append(attrs, "error_layer", failure.Layer, "reason", failure.Reason, "error_type", clean(failure.Type), "error_code", clean(failure.Code), "error_message", clean(failure.Message))
	}
	attrs = append(attrs, "outcome", outcome)
	attrs = append(attrs, extra...)
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Log(r.Context(), level, "gateway request", attrs...)
}

func (s *Server) logAttempt(r *http.Request, status int, started time.Time, retry bool) {
	t := traceFrom(r)
	if t == nil {
		return
	}
	t.addRouteHop(status, retry, started)
	if t.Capture != nil {
		clean := s.logSanitizer(t.credentials...)
		t.Capture.AddAttempt(sessions.Attempt{Timing: t.Timing, Number: t.Attempts, AccountID: t.AccountID, Status: status, StartedAt: started, DurationMS: time.Since(started).Milliseconds(), UpstreamRequestID: clean(t.UpstreamID), Retry: retry, Usage: sessionUsage(t.Usage, t.UsageFields), Failure: sessionFailure(t.Failure, clean), RequestOutputLimits: t.RequestOutputLimits, RateLimitHeaders: t.RateLimitHeaders, RateLimitHeadersTruncated: t.RateLimitHeadersTruncated})
	}
	var cached, uncached *int64
	if slices.Contains(t.UsageFields, "cached_input_tokens") {
		cached = &t.Usage.Cached
		if slices.Contains(t.UsageFields, "input_tokens") && t.Usage.Input >= t.Usage.Cached {
			n := t.Usage.Input - t.Usage.Cached
			uncached = &n
		}
	}
	s.logRecord(r, "upstream_attempt", status, time.Since(started), t.Failure,
		"attempt_started_at", started, "retry", retry,
		"request_output_limits", t.RequestOutputLimits,
		"rate_limit_headers", t.RateLimitHeaders, "rate_limit_headers_truncated", t.RateLimitHeadersTruncated,
		"usage_known", t.Usage.Known, "usage_reported_fields", t.UsageFields,
		"input_tokens", t.Usage.Input, "output_tokens", t.Usage.Output, "total_tokens", t.Usage.Total,
		"cached_input_tokens", cached, "uncached_input_tokens", uncached, "timing", t.Timing)
}

func (s *Server) logCompletion(r *http.Request, w *logResponseWriter) {
	t := w.trace
	if w.status == 0 && t.Failure.Reason != "handler_aborted" {
		w.status = http.StatusOK
	}
	s.finishSession(r, w)
	// Successful admin polling and health checks do not obscure proxy traffic.
	if w.status < 400 && (strings.HasPrefix(r.URL.Path, "/admin/") || r.URL.Path == "/healthz" || r.URL.Path == "/metrics") {
		return
	}
	s.logRecord(r, "request_completed", w.status, time.Since(t.Started), t.Failure, "response_bytes", w.bytes, "masked_spans", t.MaskedSpans)
	s.recordRoute(r, w.status, t)
}

func withTrace(r *http.Request, t *requestTrace) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), traceKey{}, t))
}
