package gateway

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// redactionSpanRecord is one masked (original -> placeholder) or restored
// (placeholder -> original) mapping kept for the admin "脱敏拦截日志" view.
type redactionSpanRecord struct {
	Time        time.Time `json:"time"`
	Kind        string    `json:"kind"`
	Label       string    `json:"label"`
	Placeholder string    `json:"placeholder"`
	Original    string    `json:"original"`
	RequestID   string    `json:"request_id,omitempty"`
}

// redactionSpanLog is a bounded ring of the most recent span records. It is
// the web-facing trace counterpart of the `redaction span` structured logs:
// same events, kept in memory (capacity-bound) for the admin console.
type redactionSpanLog struct {
	mu      sync.Mutex
	entries []redactionSpanRecord
	next    int
	full    bool
}

func newRedactionSpanLog(capacity int) *redactionSpanLog {
	if capacity < 1 {
		capacity = 1
	}
	return &redactionSpanLog{entries: make([]redactionSpanRecord, capacity)}
}

func (l *redactionSpanLog) add(rec redactionSpanRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[l.next] = rec
	l.next = (l.next + 1) % len(l.entries)
	if l.next == 0 {
		l.full = true
	}
}

// recent returns up to limit records in reverse chronological order (newest
// first), optionally filtered by kind, label and request_id.
func (l *redactionSpanLog) recent(limit int, kind, label, requestID string) []redactionSpanRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := l.next
	if l.full {
		count = len(l.entries)
	}
	if count == 0 {
		return nil
	}
	if limit <= 0 {
		limit = count
	}
	out := make([]redactionSpanRecord, 0, min(count, limit))
	for i := 0; i < count && len(out) < limit; i++ {
		idx := (l.next - 1 - i + len(l.entries)) % len(l.entries)
		rec := l.entries[idx]
		if rec.Time.IsZero() {
			continue
		}
		if kind != "" && rec.Kind != kind {
			continue
		}
		if label != "" && rec.Label != label {
			continue
		}
		if requestID != "" && !strings.Contains(rec.RequestID, requestID) {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// redactionSpansAdmin serves GET /admin/v1/redaction/spans: the recent
// masked/restored mapping trace for the web "脱敏拦截日志" tab. Supported
// query parameters: limit (default 200, capped at 1000), kind, label,
// request_id.
func (s *Server) redactionSpansAdmin(w http.ResponseWriter, r *http.Request) {
	log := s.redactionSpans
	if log == nil {
		writeJSON(w, 200, map[string]any{"data": []redactionSpanRecord{}, "limit": 0})
		return
	}
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	records := log.recent(limit, r.URL.Query().Get("kind"), r.URL.Query().Get("label"), r.URL.Query().Get("request_id"))
	// Retain the caller-requested order (newest first).
	sort.SliceStable(records, func(i, j int) bool { return records[i].Time.After(records[j].Time) })
	writeJSON(w, 200, map[string]any{"data": records, "limit": limit})
}
