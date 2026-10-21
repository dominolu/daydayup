package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"daydayup/internal/config"
	"daydayup/internal/pool"
	"daydayup/internal/sessions"
)

func (s *Server) LoadSessions() error {
	if s.Config.DBFile == "" {
		return nil
	}
	retention := sessions.Retention{Days: s.Config.Sessions.RetentionDays, MaxBodyMB: s.Config.Sessions.MaxBodyMB, BodyFiles: s.Config.Sessions.BodyFiles}
	store, err := sessions.OpenWithRetention(s.Config.DBFile, retention)
	if err == nil {
		s.Sessions = store
	}
	return err
}

func (s *Server) beginSession(w http.ResponseWriter, r *http.Request, prov, path string, body []byte, complete bool) bool {
	t := traceFrom(r)
	var payload struct {
		Previous     string          `json:"previous_response_id"`
		Conversation json.RawMessage `json:"conversation"`
		ThreadID     string          `json:"thread_id"`
	}
	_ = json.Unmarshal(body, &payload)
	// Routing safety is independent of whether archival is enabled.
	t.Stateful = payload.Previous != "" || payload.ThreadID != "" || (len(payload.Conversation) > 0 && string(payload.Conversation) != "null") || r.Header.Get("X-Codex-Turn-State") != ""
	id, identity, identityErr := resolveIdentity(r, body)
	if identityErr != nil {
		apiError(w, 400, "invalid_session_id", identityErr.Error())
		return false
	}
	if s.Sessions == nil {
		// Without archival the session id still exists so providers can
		// derive stable per-session headers (WorkBuddy conversation ids)
		// and clients still see the response contract.
		t.Purpose = "model"
		if id == "" {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err != nil {
				apiError(w, 503, "session_id_failed", "cannot generate session ID")
				return false
			}
			id = "sess-" + hex.EncodeToString(b)
			identity.Source = "generated"
			identity.Note = "no_session_identifier"
		}
		t.SessionID, t.Identity = id, identity
		w.Header().Set("X-Session-ID", id)
		w.Header().Set("X-Session-Source", identity.Source)
		w.Header().Set("X-Request-Purpose", t.Purpose)
		return true
	}
	t.Stateful = t.Stateful || r.Method != http.MethodPost || !canRetry(r.Method, path, 429)
	t.Purpose = "model"
	if complete {
		t.Title = inspectTitleRequest(r, path, body)
	}
	if t.Title != nil {
		t.Purpose = "title"
		if identity.Client == "Unknown" {
			switch {
			case strings.HasPrefix(t.Title.Detector, "dsh:"):
				identity.Client = "DSH"
			case strings.HasPrefix(t.Title.Detector, "opencode:"):
				identity.Client = "OpenCode"
			case strings.HasPrefix(t.Title.Detector, "claude-code:"):
				identity.Client = "Claude Code"
			}
		}
	}
	var userMessages []string
	if complete && t.Title == nil {
		userMessages = sessions.MessageHashes(sessions.HumanMessages(path, body))
	}
	history, previousHistory, toolIDs := requestHistory(path, body)
	// Opaque resources and server-side continuation must remain on their owner.
	conflict := false
	unknownState := false
	if payload.Previous != "" {
		sid, account, ok := s.Sessions.ResolveResponse(prov, payload.Previous)
		if ok {
			conflict = id != "" && id != sid
			if id == "" {
				id = sid
				identity.Source = "previous_response_id"
			}
			t.PinnedAccount = account
		}
	}
	if state := r.Header.Get("X-Codex-Turn-State"); state != "" {
		sid, account, ok := s.Sessions.ResolveTurnState(prov, turnStateHash(state))
		unknownState = !ok || len(r.Header.Values("X-Codex-Turn-State")) != 1
		if ok && !unknownState {
			conflict = conflict || id != "" && id != sid || t.PinnedAccount != "" && t.PinnedAccount != account
			if id == "" {
				id, identity.Source = sid, "upstream_turn_state"
			}
			t.PinnedAccount = account
		}
	}
	if t.Title != nil {
		if id != "" {
			t.Title.Association = "session_id"
		} else if !t.Stateful {
			var linked string
			id, linked, t.Title.Association = s.Sessions.ResolveTitleSession(prov, identity.Client, t.Title.SourceMessages)
			if id != "" {
				identity.Source, identity.LinkedRequestID = "title_context", linked
				t.Title.LinkedRequestID = linked
			}
		} else {
			t.Title.Association = "unmatched"
		}
	}
	if t.Title == nil && id == "" && !t.Stateful && complete && previousHistory != "" {
		var linked string
		id, linked, identity.Note = s.Sessions.ResolveToolContinuation(prov, path, previousHistory, toolIDs)
		if id != "" {
			identity.Source, identity.LinkedRequestID = "tool_continuation", linked
		}
	}
	if t.Title == nil && id == "" && !t.Stateful && complete {
		if seed, linked := s.Sessions.ResolveTitleSeed(prov, identity.Client, userMessages, t.ID); seed != "" {
			id = seed
			identity.Source, identity.LinkedRequestID = "title_seed", linked
		}
	}
	if id == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			apiError(w, 503, "session_id_failed", "cannot generate session ID")
			return false
		}
		id = "sess-" + hex.EncodeToString(b)
		identity.Source = "generated"
		if identity.Note == "" {
			identity.Note = "no_session_identifier"
		}
	}
	t.SessionID = id
	t.Identity = identity
	w.Header().Set("X-Session-ID", id)
	w.Header().Set("X-Session-Source", identity.Source)
	w.Header().Set("X-Request-Purpose", t.Purpose)
	query := r.URL.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || lower == "authorization" {
			query.Set(key, "[REDACTED]")
		}
	}
	record := sessions.Record{Purpose: t.Purpose, Title: t.Title, UserMessages: userMessages, Identity: identity, HistoryHash: history, ID: t.ID, SessionID: id, StartedAt: t.Started, Method: r.Method, Path: path, Query: query.Encode(), Provider: prov, Group: t.Group, Model: t.Model, Request: sessions.Body{ContentType: r.Header.Get("Content-Type"), Complete: complete, Headers: archiveHeaders(r.Header)}}
	capture, err := s.Sessions.Begin(record, body, "")
	if err != nil {
		slog.Error("session archive begin failed", "request_id", t.ID)
		apiError(w, 503, "session_archive_failed", "cannot durably archive request; check session storage")
		return false
	}
	t.Capture = capture
	if conflict {
		apiError(w, 409, "session_conflict", "upstream continuation belongs to a different session or account")
		return false
	}
	if unknownState {
		apiError(w, 409, "unknown_upstream_state", "upstream turn state has no known account binding; start a fresh turn without that state")
		return false
	}
	if payload.Previous != "" && t.PinnedAccount == "" {
		t.PinnedAccount = s.Sessions.PreferredAccount(id, t.Model)
		if t.PinnedAccount == "" {
			apiError(w, 409, "unknown_response_session", "previous_response_id is unknown; supply the X-Session-ID used when the upstream response was created")
			return false
		}
	}
	return true
}

func archiveHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	keys := []string{"Content-Type", "Content-Encoding", "Accept", "X-Session-ID", "X-Session-Source", "X-Request-Purpose"}
	for _, header := range nativeSessionHeaders {
		keys = append(keys, header.Name)
	}
	for _, key := range keys {
		if value := h.Get(key); value != "" {
			if len(value) <= 512 {
				out[key] = value
			}
		}
	}
	return out
}

func (s *Server) acquireSessionAccount(t *requestTrace, prov, group, model string, excluded map[string]bool) (config.Account, error) {
	preferred := t.PinnedAccount
	if t.RefreshRetryID != "" {
		preferred = t.RefreshRetryID
		t.RefreshRetryID = ""
	}
	if s.Sessions == nil || t.Capture == nil {
		return s.Pool.AcquireGroup(prov, t.Realm, group, model, excluded, preferred, t.Stateful && preferred != "")
	}
	unlock := s.Sessions.LockRoute(t.SessionID)
	defer unlock()
	// The session pin follows the model, not the platform: with cross-platform
	// routing the same model id stays on the same account even when another
	// platform could also serve it.
	sessPreferred := s.Sessions.PreferredAccount(t.SessionID, model)
	if t.PinnedAccount != "" {
		sessPreferred = t.PinnedAccount
	}
	if t.RefreshRetryID != "" {
		sessPreferred = t.RefreshRetryID
		t.RefreshRetryID = ""
	}
	preferred = sessPreferred
	a, err := s.Pool.AcquireGroup(prov, t.Realm, group, model, excluded, preferred, t.Stateful && preferred != "")
	if err != nil {
		return a, err
	}
	if err = s.Sessions.BindAccount(t.SessionID, model, a.ID); err != nil {
		s.Pool.Finish(a.ID, 0, pool.Usage{}, true, 0, model)
		return config.Account{}, errors.New("session account binding could not be saved")
	}
	return a, nil
}

func sessionUsage(u pool.Usage, fields []string) sessions.Usage {
	return sessions.Usage{Reported: fields, Input: u.Input, Output: u.Output, Total: u.Total, Cached: u.Cached, Known: u.Known}
}
func sessionFailure(d diagnostic, clean func(string) string) sessions.Failure {
	return sessions.Failure{Layer: d.Layer, Reason: d.Reason, Type: clean(d.Type), Code: clean(d.Code), Message: clean(d.Message)}
}
func (s *Server) finishSession(r *http.Request, w *logResponseWriter) {
	t := w.trace
	if t.Capture == nil {
		return
	}
	outcome := "success"
	if w.status >= 400 || t.Failure.Reason != "" || t.Incomplete {
		outcome = "error"
	}
	response := sessions.Body{ContentType: w.Header().Get("Content-Type"), Complete: !t.Incomplete, Headers: archiveHeaders(w.Header())}
	if t.Incomplete {
		response.Error = "response ended before completion"
	}
	if err := t.Capture.Finish(sessions.Result{PoolWaitMS: t.PoolWaitMS, PoolWaits: t.PoolWaits, GeneratedTitle: t.GeneratedTitle, TitleError: t.TitleError, TurnStateHash: turnStateHash(w.Header().Get("X-Codex-Turn-State")), ToolCallIDs: t.ToolCallIDs, Status: w.status, Outcome: outcome, Failure: sessionFailure(t.Failure, s.logSanitizer(t.credentials...)), Response: response, ResponseID: t.ResponseID, Provider: t.Provider}); err != nil {
		slog.Error("session archive completion failed", "request_id", t.ID, "session_id", t.SessionID)
	}
	if t.Title != nil {
		if record, err := s.Sessions.Record(t.SessionID, t.ID); err == nil {
			t.Title = record.Title
		}
	}
}

func pagination(r *http.Request) (int, int, error) {
	offset, limit := 0, 30
	var err error
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 {
			return 0, 0, errors.New("invalid offset")
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, errors.New("limit must be 1–100")
		}
	}
	return offset, limit, nil
}

func (s *Server) sessionAdmin(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/admin/v1/sessions"
	if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if s.Sessions == nil {
		apiError(w, 503, "session_storage_disabled", "session storage is disabled")
		return true
	}
	offset, limit, err := pagination(r)
	if err != nil {
		apiError(w, 400, "invalid_pagination", err.Error())
		return true
	}
	if r.URL.Path == prefix && r.Method == http.MethodGet {
		archived := r.URL.Query().Get("archived")
		if archived != "" && archived != "true" && archived != "false" {
			apiError(w, 400, "invalid_filter", "archived must be true or false")
			return true
		}
		rows, total := s.Sessions.List(r.URL.Query().Get("q"), archived == "true", offset, limit)
		writeJSON(w, 200, map[string]any{"data": rows, "total": total, "offset": offset, "limit": limit})
		return true
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix+"/"), "/")
	if len(parts) == 0 || !sessions.ValidID(parts[0]) {
		apiError(w, 404, "session_not_found", "session not found")
		return true
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		summary, rows, total, e := s.Sessions.Detail(id, offset, limit)
		if e != nil {
			apiError(w, 404, "session_not_found", "session not found")
			return true
		}
		tools, count, _ := s.Sessions.Tools(id, "", 0, 30)
		writeJSON(w, 200, map[string]any{"data": summary, "requests": rows, "total": total, "offset": offset, "limit": limit, "tool_events": tools, "tool_events_total": count})
		return true
	}
	if len(parts) == 2 && parts[1] == "tool-events" && r.Method == http.MethodGet {
		rows, total, e := s.Sessions.Tools(id, r.URL.Query().Get("request_id"), offset, limit)
		if e != nil {
			apiError(w, 404, "session_not_found", "session or request not found")
			return true
		}
		writeJSON(w, 200, map[string]any{"data": rows, "total": total, "offset": offset, "limit": limit})
		return true
	}
	if len(parts) == 1 && r.Method == http.MethodPut {
		var change struct {
			Name     *string `json:"name"`
			Archived *bool   `json:"archived"`
		}
		if !decodeJSON(w, r, &change) {
			return true
		}
		if change.Name == nil && change.Archived == nil {
			apiError(w, 400, "invalid_request", "name or archived is required")
			return true
		}
		if err = s.Sessions.Update(id, change.Name, change.Archived); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				apiError(w, 404, "session_not_found", "session not found")
			} else {
				apiError(w, 400, "session_update_failed", "could not save session; name must contain 1–120 characters without line breaks")
			}
			return true
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return true
	}
	if (len(parts) == 3 || len(parts) == 4) && parts[1] == "requests" && sessions.ValidID(parts[2]) && r.Method == http.MethodGet {
		record, e := s.Sessions.Record(id, parts[2])
		if e != nil {
			apiError(w, 404, "request_not_found", "session request not found")
			return true
		}
		if len(parts) == 3 {
			tools, count, _ := s.Sessions.Tools(id, parts[2], 0, 30)
			writeJSON(w, 200, map[string]any{"data": record, "tool_events": tools, "tool_events_total": count, "request_preview": s.bodyPreview(id, parts[2], "request"), "response_preview": s.bodyPreview(id, parts[2], "response")})
			return true
		}
		direction := parts[3]
		if direction != "request" && direction != "response" {
			apiError(w, 404, "not_found", "body not found")
			return true
		}
		if direction == "response" && record.Outcome == "running" {
			apiError(w, 409, "response_running", "response is still being recorded")
			return true
		}
		body, e := s.Sessions.ReadBody(id, parts[2], direction)
		if e != nil {
			apiError(w, 500, "archive_unreadable", "encrypted body could not be opened")
			return true
		}
		defer body.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.bin"`, parts[2], direction))
		w.WriteHeader(200)
		if _, e = io.Copy(w, body); e != nil {
			panic(http.ErrAbortHandler)
		}
		return true
	}
	apiError(w, 404, "not_found", "session endpoint not found")
	return true
}

func (s *Server) bodyPreview(id, requestID, direction string) map[string]any {
	body, err := s.Sessions.ReadBody(id, requestID, direction)
	if err != nil {
		return map[string]any{"text": "", "unavailable": true}
	}
	defer body.Close()
	const limit = 64 << 10
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	truncated := len(b) > limit
	if truncated {
		b = b[:limit]
	}
	binary := !utf8.Valid(b) || strings.ContainsRune(string(b), '\x00')
	text := string(b)
	if binary {
		text = "二进制内容，请下载完整正文。"
	}
	return map[string]any{"text": text, "truncated": truncated, "binary": binary, "incomplete": err != nil}
}
