package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/observe"
	"daydayup/internal/pool"
	"daydayup/internal/provider"
)

// isGenerationPath reports the stateless generation endpoints whose explicit
// upstream rejections may be replayed on another account or candidate.
func isGenerationPath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/images/generations", "/v1/images/edits":
		return true
	}
	return false
}

// Only stateless generation endpoints may fail over after an explicit rejection.
// Transport errors and 5xx on POST are ambiguous and may already be billable.
func canRetry(method, path string, status int) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return status == 401 || status == 429 || status == 502 || status == 503 || status == 504
	}
	if method != http.MethodPost || (status != 401 && status != 429) {
		return false
	}
	return isGenerationPath(path)
}

// nextLocalReset returns the next 04:00 local wall clock, when the WorkBuddy
// credit windows roll over (reference gateway parks out-of-credits accounts
// until that boundary).
func nextLocalReset() time.Time {
	now := time.Now()
	reset := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	if !reset.After(now) {
		reset = reset.AddDate(0, 0, 1)
	}
	return reset
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, prov, path string, body []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.Config.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	model := config.CanonicalModel(s.Config.ModelRename, modelFrom(r, body))
	trace := traceFrom(r)
	trace.Model = model
	trace.RequestOutputLimits = requestOutputLimits(r, body)
	excluded := map[string]bool{}
	attempt, boundedAttempts, rateRejections := 0, 0, 0
	for {
		if ctx.Err() != nil {
			proxyContextError(w, r, false)
			return
		}
		a, err := s.acquireWaitingAccount(ctx, r, prov, trace.Group, model, excluded)
		if err != nil {
			if ctx.Err() != nil {
				proxyContextError(w, r, true)
				return
			}
			if errors.Is(err, errWaitQueueFull) {
				w.Header().Set("Retry-After", "1")
				apiError(w, 503, "pool_queue_full", err.Error())
				return
			}
			if errors.Is(err, errPoolWaitTimeout) {
				// Stop paying for a rate-limited account out of the generation
				// budget: fail fast so the client can retry once capacity
				// returns instead of timing out mid-request.
				w.Header().Set("Retry-After", "5")
				apiError(w, 503, "pool_wait_budget_exhausted", "no upstream capacity or rate-limit recovery within the pool wait budget; retry later")
				trace.Failure.Layer = "pool"
				return
			}
			var selection *pool.AcquireError
			if errors.As(err, &selection) {
				trace.rejections = selection.Rejections
				if selection.OnlyReason(pool.ReasonModelDenied) {
					apiError(w, 403, pool.ReasonModelDenied, "the requested model is not available in the current token plan for any configured account; select an available model from GET /v1/models")
					return
				}
			}
			w.Header().Set("Retry-After", "5")
			apiError(w, 503, "pool_unavailable", err.Error())
			return
		}
		excluded[a.ID] = true
		attempt++
		boundedAttempts++
		started := time.Now()
		trace.AccountID, trace.Attempts = a.ID, attempt
		// The platform of the chosen account is the effective provider: with
		// global routing the same model may be served by any platform, so the
		// trace (and this attempt) records which platform actually answered.
		trace.Provider = a.Provider
		trace.UpstreamID, trace.UpstreamModel, trace.Failure, trace.Usage = "", "", diagnostic{}, pool.Usage{}
		trace.UsageFields = nil
		trace.Timing = nil
		trace.RateLimitHeaders, trace.RateLimitHeadersTruncated = nil, false
		trace.credentials = append(trace.credentials, a.APIKey, a.QuotaFeedKey, a.RefreshToken)
		p, _ := s.providers.Get(a.Provider)
		// model_rename: the answering platform receives the id it actually
		// advertises while the client keeps the canonical name in requests,
		// routing, /v1/models and response model fields.
		upstreamModel := config.UpstreamModel(s.Config.ModelRename, a.Provider, model)
		trace.UpstreamModel = upstreamModel
		attemptBody := body
		if upstreamModel != model {
			attemptBody = rewriteModelField(body, upstreamModel)
		}
		// Optional per-provider request body rewrite (e.g. WorkBuddy gzip for
		// the CLI/VSCode identities). Deterministic per input, so each retry
		// attempt rebuilds it safely.
		upstreamBody := attemptBody
		transformer, transformable := p.(provider.BodyTransformer)
		if transformable {
			upstreamBody, err = transformer.TransformRequestBody(r.Method, path, r.Header.Get("Content-Type"), attemptBody, a)
			if err != nil {
				s.Pool.Finish(a.ID, 0, pool.Usage{}, true, 0, model)
				trace.Failure = diagnostic{Layer: "gateway", Reason: "body_transform_failed", Message: "request body could not be transformed for the upstream identity"}
				s.logAttempt(r, 0, started, false)
				apiError(w, 400, "body_transform_failed", "request body could not be transformed for the upstream identity")
				return
			}
		}
		// Optional per-provider path rewrite (WorkBuddy serves chat at
		// /v2/chat/completions while clients call /v1/chat/completions).
		upstreamPath := path
		if rewriter, ok := p.(provider.PathRewriter); ok {
			upstreamPath, _ = rewriter.RewritePath(path)
		}
		target, _ := url.Parse(a.BaseURL)
		target.Path = upstreamPath
		rawPath := r.URL.EscapedPath()
		if strings.HasPrefix(r.URL.Path, "/providers/") {
			prefix := "/providers/" + prov
			rawPath = strings.TrimPrefix(rawPath, prefix)
		}
		if decoded, e := url.PathUnescape(rawPath); e == nil && decoded == path && upstreamPath == path {
			target.RawPath = rawPath
		}
		target.RawQuery = r.URL.RawQuery
		req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(upstreamBody))
		if err != nil {
			s.Pool.Finish(a.ID, 0, pool.Usage{}, true, 0, model)
			trace.Failure = diagnostic{Layer: "gateway", Reason: "invalid_request", Message: "cannot construct upstream request"}
			s.logAttempt(r, 0, started, false)
			apiError(w, 400, "invalid_request", "cannot construct upstream request")
			return
		}
		req.Header = requestHeaders(r.Header)
		if transformable {
			for key, values := range transformer.BodyEncodingHeaders(a) {
				req.Header.Del(key)
				for _, v := range values {
					req.Header.Add(key, v)
				}
			}
		}
		req.GetBody = nil
		if sessionAuth, ok := p.(provider.SessionAuthorizer); ok {
			sessionAuth.AuthorizeSession(req, a, trace.SessionID, model)
		} else {
			p.Authorize(req, a)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			if resp != nil {
				resp.Body.Close()
			}
			s.Pool.Finish(a.ID, 0, pool.Usage{}, true, 5*time.Second, model)
			retry := !trace.Stateful && (r.Method == http.MethodGet || r.Method == http.MethodHead) && boundedAttempts < s.Config.MaxAttempts && ctx.Err() == nil
			trace.Failure = diagnostic{Layer: "transport", Reason: "upstream_unreachable", Message: "upstream request failed"}
			if ctx.Err() != nil {
				trace.Failure.Reason = "upstream_timeout"
			}
			if r.Context().Err() != nil {
				trace.Failure.Layer, trace.Failure.Reason = "client", "client_cancelled"
			}
			s.logAttempt(r, 0, started, retry)
			if retry {
				continue
			}
			status := 502
			code := "upstream_unreachable"
			message := "upstream request failed"
			switch {
			case r.Context().Err() != nil:
				// The client went away. 499 says so and avoids blaming our own
				// deadline when both fired at the same moment.
				status, code, message = 499, "client_cancelled", "client cancelled the request"
			case ctx.Err() != nil:
				status, code, message = 504, "upstream_timeout", "request deadline exceeded while waiting for the upstream response"
			}
			failure := trace.Failure
			apiError(w, status, code, message)
			trace.Failure = failure
			return
		}
		trace.RateLimitHeaders, trace.RateLimitHeadersTruncated = observeRateLimitHeaders(resp.Header, s.logSanitizer(trace.credentials...))
		delay := cooldown(resp.StatusCode, resp.Header.Get("Retry-After"))
		if resp.StatusCode == http.StatusTooManyRequests {
			rateRejections++
			delay = rateLimitDelay(resp.Header.Get("Retry-After"), rateRejections)
		}
		trace.UpstreamID = resp.Header.Get("X-Request-ID")
		if trace.UpstreamID == "" {
			trace.UpstreamID = resp.Header.Get("Request-ID")
		}
		// modelDenied marks the one 403 family that is scoped to (account,
		// model) instead of to the credential: this account proved it cannot
		// serve the model, but another candidate in the routing group (possibly
		// on another platform) still can. It is handled as a routing
		// rejection below instead of being forwarded to the client.
		modelDenied := false
		if resp.StatusCode >= 400 {
			// Inspect a bounded copy and restore every byte before forwarding.
			original := resp.Body
			prefix, readErr := io.ReadAll(io.LimitReader(original, 64<<10))
			resp.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(prefix), &onceErrorReader{err: readErr}, original), Closer: original}
			trace.Failure = upstreamDiagnostic(resp.StatusCode, protocolError(prefix))
			rejected := observe.NewObserver(false, false)
			rejected.Write(prefix)
			rejected.Done()
			trace.Usage = rejected.Usage()
			trace.UsageFields = rejected.Reported()
			if model != "" && resp.StatusCode == http.StatusTooManyRequests && readErr == nil && p.ModelRateLimit(prefix) {
				s.Pool.ThrottleModel(a.ID, model, delay)
				delay = 0
			}
			if resp.StatusCode == http.StatusForbidden && readErr == nil {
				restricted := false
				if restricter, ok := p.(provider.Restriction); ok && restricter.Restriction(prefix) {
					restricted = true
					// WorkBuddy 11140: an account-level abuse restriction
					// implicates the whole realm through the derived device.
					s.Pool.HoldRealm(a.Provider, pool.Realm(a), 30*time.Minute)
				}
				if model != "" && p.ModelUnavailable(prefix) {
					s.Pool.DenyModel(a.ID, model, 15*time.Minute)
					// An account-level restriction is not a model-permission
					// verdict, and unknown write paths are never replayed, so
					// only generation requests fail over on this rejection.
					modelDenied = !restricted && isGenerationPath(path)
				}
			}
			if resp.StatusCode == http.StatusPaymentRequired && readErr == nil {
				if exhausted, ok := p.(provider.QuotaExhausted); ok && exhausted.QuotaExhausted(prefix) {
					s.Pool.MarkQuotaExhausted(a.ID, nextLocalReset())
				}
			}
		}
		quotaRejected := resp.StatusCode == http.StatusPaymentRequired
		rateRetry := resp.StatusCode == http.StatusTooManyRequests && canRetry(r.Method, path, resp.StatusCode)
		failover := !trace.Stateful && (canRetry(r.Method, path, resp.StatusCode) || quotaRejected) && boundedAttempts < s.Config.MaxAttempts && len(excluded) < len(s.Pool.Accounts())
		// OAuth providers refresh the access token on an upstream 401 and
		// retry the same account once (stateful sessions included; the
		// upstream state belongs to that account only).
		refreshRetry := false
		if resp.StatusCode == http.StatusUnauthorized && !trace.RefreshAttempted {
			trace.RefreshAttempted = true
			if _, ok, err := s.refreshAccountTokens(r.Context(), a); err == nil && ok {
				refreshRetry = true
				// Keep the pinned id so the retry reuses the fresh account.
				trace.RefreshRetryID = a.ID
			}
		}
		// A confirmed model-permission rejection rules out this account only.
		// The request produced no generation, so retrying it on another
		// candidate is safe and must not consume the generation retry budget:
		// model_not_in_token_plan is reported to the client only after every
		// candidate in the routing group has been ruled out.
		if modelDenied {
			// Bound draining so an error response cannot hold a slot indefinitely.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			s.Pool.Finish(a.ID, resp.StatusCode, trace.Usage, true, delay, model)
			// The account is now in the model-permission cache, so dropping the
			// per-request exclusion cannot reselect it; it only keeps the final
			// scheduling error reason at model_not_in_token_plan.
			delete(excluded, a.ID)
			boundedAttempts--
			s.logAttempt(r, resp.StatusCode, started, true)
			continue
		}
		if rateRetry || failover || refreshRetry {
			// Bound draining so an error response cannot hold a slot indefinitely.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			s.Pool.Finish(a.ID, resp.StatusCode, trace.Usage, true, delay, model)
			if rateRetry {
				// A rejected generation has no result to duplicate. Keep retrying
				// it under the shared cooldown until this request's context ends.
				delete(excluded, a.ID)
				boundedAttempts--
				if trace.Stateful {
					trace.PinnedAccount = a.ID
				}
			}
			if refreshRetry {
				delete(excluded, a.ID)
				// Finish marked the 401 invalid; the refresh proves the new
				// credential is valid, so restore schedulability.
				s.Pool.SetCredentialValid(a.ID)
			}
			s.logAttempt(r, resp.StatusCode, started, true)
			continue
		}
		ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		observeCT := ct == "text/event-stream" || ct == "application/json" || strings.HasSuffix(ct, "+json")
		restoreCT := observeCT || strings.HasPrefix(ct, "text/")
		if s.redact != nil && restoreCT {
			if err := decodeRedactionResponse(resp); err != nil {
				resp.Body.Close()
				s.Pool.Finish(a.ID, 502, pool.Usage{}, true, 0, model)
				trace.Failure = diagnostic{Layer: "gateway", Reason: "response_decode_failed", Message: "upstream response could not be decoded"}
				s.logAttempt(r, 502, started, false)
				apiError(w, 502, "response_decode_failed", trace.Failure.Message)
				return
			}
		}
		h := resp.Header.Clone()
		stripHeaders(h)
		h.Del("Set-Cookie")
		h.Del("X-Request-ID")
		h.Del("X-Session-ID")
		h.Del("X-Session-Source")
		h.Del("X-Request-Purpose")
		if s.redact != nil && restoreCT {
			for _, key := range []string{"Content-Length", "Content-MD5", "Digest", "ETag"} {
				h.Del(key)
			}
		}
		for key, values := range h {
			w.Header()[key] = values
		}
		o := observe.NewObserver(ct == "text/event-stream", trace.Title != nil)
		// Streaming bridge/middleware writers: if the client asked for
		// streaming but the upstream answered with plain JSON (some providers
		// ignore stream:true), the SSE translator would swallow the whole body
		// — pass it through instead so the agent sees a concrete response.
		if sw, ok := w.(rawSwitcher); ok && sw.wantsStream() && !o.IsSSE() {
			sw.setRaw()
		}
		if o.IsSSE() {
			w.Header().Set("X-Accel-Buffering", "no")
		}
		w.WriteHeader(resp.StatusCode)
		var copyErr error
		if o.IsSSE() {
			copyErr = http.NewResponseController(w).Flush()
			if copyErr == nil {
				if s.redact != nil {
					copyErr = observe.CopySSERestored(r.Context(), w, resp.Body, o, observe.SSERestoreHooks{Engine: s.redact, OnError: s.redactRecordError, OnFindings: s.logToolFindings})
				} else {
					copyErr = observe.CopySSE(w, resp.Body, o)
				}
			}
			o.Done()
			trace.StreamTerminalReceived = o.Terminal()
			trace.StreamTerminalForwarded = o.Terminal() && copyErr == nil
		} else if observeCT || (s.redact != nil && restoreCT) {
			reader := io.TeeReader(resp.Body, o)
			if s.redact != nil {
				copyErr = s.copyRestored(r.Context(), w, reader, observeCT)
			} else {
				_, copyErr = io.Copy(w, reader)
			}
			o.Done()
		} else {
			_, copyErr = io.Copy(w, resp.Body)
		}
		resp.Body.Close()
		trace.ToolCallIDs = o.ToolCallIDs()
		if trace.Title != nil {
			trace.GeneratedTitle, trace.TitleError = o.TitleResult()
			if !observeCT || o.ObservationGap() || o.TooLarge() {
				trace.GeneratedTitle, trace.TitleError = "", "title_observation_incomplete"
			}
		}
		failed := resp.StatusCode >= 400 || copyErr != nil || o.Failed() || (o.IsSSE() && !o.Terminal())
		s.Pool.Finish(a.ID, resp.StatusCode, o.Usage(), failed, delay, model)
		trace.Timing = o.Timing(started, !failed)
		trace.Usage = o.Usage()
		trace.UsageFields = o.Reported()
		trace.ResponseID = o.ResponseID()
		truncated := copyErr != nil || (o.IsSSE() && !o.Terminal())
		trace.Incomplete = truncated
		if resp.StatusCode < 400 && o.Failed() {
			trace.Failure = upstreamDiagnostic(resp.StatusCode, o.Error())
		}
		if trace.Failure.Reason == "" && truncated {
			trace.Failure = streamStopDiagnostic(r, ctx)
			// The protocol terminal never arrived. Keep the concrete read error
			// (EOF vs reset vs our own deadline) out of the client-facing
			// message but in the log: without it "response ended before
			// completion" cannot be triaged.
			s.logRecord(r, "stream_copy_failed", resp.StatusCode, time.Since(started), trace.Failure,
				"read_error", streamErrorClass(copyErr),
				"read_error_message", s.logSanitizer(trace.credentials...)(streamErrorMessage(copyErr)))
		}
		s.logAttempt(r, resp.StatusCode, started, false)
		// A partial response must terminate the downstream connection. Returning
		// normally could make a truncated SSE or JSON payload look successful.
		if truncated {
			panic(http.ErrAbortHandler)
		}
		return
	}
}

type replayBody struct {
	io.Reader
	io.Closer
}
type onceErrorReader struct{ err error }

func (r *onceErrorReader) Read([]byte) (int, error) {
	if r.err != nil {
		err := r.err
		r.err = nil
		return 0, err
	}
	return 0, io.EOF
}
