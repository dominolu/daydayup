package gateway

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"safuno/internal/sessions"
)

func (s *Server) toolReport(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/gateway/v1/sessions/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) != 2 || parts[1] != "tool-events" || !sessions.ValidID(parts[0]) || r.Method != http.MethodPost {
		apiError(w, 404, "not_found", "tool event endpoint not found")
		return true
	}
	id := parts[0]
	if s.Sessions == nil {
		apiError(w, 503, "session_storage_disabled", "session storage is disabled")
		return true
	}
	var body struct {
		ID         string   `json:"id"`
		RequestID  string   `json:"request_id"`
		Name       string   `json:"name"`
		DurationMS *float64 `json:"duration_ms"`
		Status     string   `json:"status"`
	}
	if !decodeJSON(w, r, &body) {
		return true
	}
	if body.DurationMS == nil {
		apiError(w, 400, "invalid_tool_event", "duration_ms is required")
		return true
	}
	created, err := s.Sessions.ReportTool(id, sessions.ToolEvent{ID: body.ID, RequestID: body.RequestID, Name: body.Name, DurationMS: *body.DurationMS, Status: body.Status})
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			apiError(w, 404, "request_not_found", "session or associated request not found")
		case errors.Is(err, sessions.ErrInvalidTool):
			apiError(w, 400, "invalid_tool_event", "valid id, request_id, duration_ms (0–86400000) and status (success/error/cancelled) are required")
		case errors.Is(err, sessions.ErrToolConflict):
			apiError(w, 409, "tool_event_conflict", "tool event ID already exists with different data")
		case errors.Is(err, sessions.ErrToolLimit):
			apiError(w, 409, "tool_event_limit", "session has reached its tool event limit")
		default:
			apiError(w, 503, "tool_event_save_failed", "could not durably save tool event")
		}
		return true
	}
	traceFrom(r).SessionID = id
	w.Header().Set("X-Session-ID", id)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"ok": true, "created": created, "source": "client_reported"})
	return true
}
