package sessions

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

var ErrToolConflict = errors.New("tool event ID already exists with different data")
var ErrInvalidTool = errors.New("invalid tool event")
var ErrToolLimit = errors.New("tool event limit reached")

type ToolEvent struct {
	ID         string    `json:"id"`
	RequestID  string    `json:"request_id"`
	Name       string    `json:"name"`
	DurationMS float64   `json:"duration_ms"`
	Status     string    `json:"status"`
	ReportedAt time.Time `json:"reported_at"`
	Source     string    `json:"source"`
}

func validTool(t ToolEvent) bool {
	return ValidID(t.ID) && ValidID(t.RequestID) && len([]rune(t.Name)) <= 128 && !strings.ContainsAny(t.Name, "\r\n\x00") && !math.IsNaN(t.DurationMS) && !math.IsInf(t.DurationMS, 0) && t.DurationMS >= 0 && t.DurationMS <= 86400000 && (t.Status == "success" || t.Status == "error" || t.Status == "cancelled")
}

// loadAllTools reads every tool event from the database, grouped by session.
func (s *Store) loadAllTools() (map[string]map[string]ToolEvent, error) {
	rows, err := s.db.SQL().Query(`SELECT session_id, tool_id, event FROM session_tools`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]ToolEvent{}
	for rows.Next() {
		var sessionID, toolID string
		var sealed []byte
		if err := rows.Scan(&sessionID, &toolID, &sealed); err != nil {
			return nil, err
		}
		plain, err := s.db.Open(sealed, "stool:"+sessionID+"/"+toolID)
		if err != nil {
			return nil, err
		}
		var tool ToolEvent
		if err := json.Unmarshal([]byte(plain), &tool); err != nil {
			return nil, ErrInvalidTool
		}
		if !validTool(tool) || tool.ID != toolID || tool.Source != "client_reported" || tool.ReportedAt.IsZero() {
			return nil, ErrInvalidTool
		}
		if out[sessionID] == nil {
			out[sessionID] = map[string]ToolEvent{}
		}
		out[sessionID][tool.ID] = tool
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ReportTool is idempotent by session and tool-call ID; it never changes usage.
func (s *Store) ReportTool(id string, tool ToolEvent) (bool, error) {
	if tool.Status == "" {
		tool.Status = "success"
	}
	if !ValidID(id) || !validTool(tool) {
		return false, ErrInvalidTool
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return false, os.ErrNotExist
	}
	if _, ok := e.records[tool.RequestID]; !ok {
		return false, os.ErrNotExist
	}
	if old, ok := e.tools[tool.ID]; ok {
		if old.RequestID != tool.RequestID || old.Name != tool.Name || old.DurationMS != tool.DurationMS || old.Status != tool.Status {
			return false, ErrToolConflict
		}
		return false, nil
	}
	if len(e.tools) >= 10000 {
		return false, ErrToolLimit
	}
	tool.Source = "client_reported"
	tool.ReportedAt = time.Now()
	plain, err := json.Marshal(tool)
	if err != nil {
		return false, err
	}
	sealed, err := s.db.Seal(string(plain), "stool:"+id+"/"+tool.ID)
	if err != nil {
		return false, err
	}
	if _, err := s.db.SQL().Exec(`INSERT INTO session_tools(session_id, tool_id, event) VALUES(?, ?, ?) ON CONFLICT(session_id, tool_id) DO UPDATE SET event = excluded.event`, id, tool.ID, sealed); err != nil {
		return false, err
	}
	e.tools[tool.ID] = tool
	return true, nil
}

func (s *Store) Tools(id, requestID string, offset, limit int) ([]ToolEvent, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return nil, 0, os.ErrNotExist
	}
	if requestID != "" {
		if _, ok := e.records[requestID]; !ok {
			return nil, 0, os.ErrNotExist
		}
	}
	rows := []ToolEvent{}
	for _, tool := range e.tools {
		if requestID == "" || tool.RequestID == requestID {
			rows = append(rows, tool)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ReportedAt.Equal(rows[j].ReportedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].ReportedAt.After(rows[j].ReportedAt)
	})
	total := len(rows)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	if limit < 1 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return append([]ToolEvent{}, rows[offset:end]...), total, nil
}
