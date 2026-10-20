package sessions

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

func summarize(e *entry) Summary {
	s := Summary{ID: e.meta.ID, Name: e.meta.Name, Archived: e.meta.Archived, CreatedAt: e.meta.CreatedAt, UpdatedAt: e.meta.CreatedAt, PreferredAccounts: e.meta.PreferredAccounts, Models: []string{}, Clients: []string{}, IdentitySources: []string{}}
	s.NameInfo = defaultNameInfo(e.meta)
	if s.NameInfo.UpdatedAt.After(s.UpdatedAt) {
		s.UpdatedAt = s.NameInfo.UpdatedAt
	}
	models := map[string]bool{}
	for _, r := range e.records {
		if r.Purpose == "title" {
			s.TitleRequests++
			s.TitleUsage.Input += r.Usage.Input
			s.TitleUsage.Output += r.Usage.Output
			s.TitleUsage.Total += r.Usage.Total
			s.TitleUsage.Cached += r.Usage.Cached
			s.TitleUsage.Known = s.TitleUsage.Known || r.Usage.Known
			s.TitleUsage.Reported = mergeFields(s.TitleUsage.Reported, r.Usage.Reported)
		}
		source := r.Identity.Source
		if source == "" {
			source = "legacy_unknown"
		}
		s.IdentitySources = mergeFields(s.IdentitySources, []string{source})
		if r.Identity.Client != "" {
			s.Clients = mergeFields(s.Clients, []string{r.Identity.Client})
		}
		if source == "generated" {
			s.GeneratedRequests++
		}
		if source == "tool_continuation" {
			s.LinkedRequests++
		}
		r = decorate(r, nil)
		mergeInsights(&s, r)
		s.Requests++
		if r.Outcome == "running" {
			s.InFlight++
		} else if r.Outcome != "success" {
			s.Failures++
		}
		if (len(r.Attempts) > 0 && !r.UsageComplete && r.Outcome != "running") || r.Outcome == "interrupted" {
			s.UnknownUsageRequests++
		}
		s.UnknownAttempts += r.UnknownAttempts
		s.Usage.Input += r.Usage.Input
		s.Usage.Output += r.Usage.Output
		s.Usage.Total += r.Usage.Total
		s.Usage.Cached += r.Usage.Cached
		s.Usage.Reported = mergeFields(s.Usage.Reported, r.Usage.Reported)
		s.Usage.Known = s.Usage.Known || r.Usage.Known
		if r.Model != "" {
			models[r.Model] = true
		}
		if r.StartedAt.After(s.UpdatedAt) {
			s.UpdatedAt = r.StartedAt
		}
		if r.FinishedAt.After(s.UpdatedAt) {
			s.UpdatedAt = r.FinishedAt
		}
	}
	for _, tool := range e.tools {
		if tool.ReportedAt.After(s.UpdatedAt) {
			s.UpdatedAt = tool.ReportedAt
		}
		addTool(&s.Performance, tool)
	}
	for model := range models {
		s.Models = append(s.Models, model)
	}
	sort.Strings(s.Models)
	sort.Strings(s.Clients)
	sort.Strings(s.IdentitySources)
	return s
}

func (s *Store) List(query string, archived bool, offset, limit int) ([]Summary, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var results []Summary
	query = strings.ToLower(query)
	for _, e := range s.entries {
		summary := summarize(e)
		if summary.Archived != archived || !strings.Contains(strings.ToLower(summary.ID+" "+summary.Name+" "+strings.Join(summary.Models, " ")+" "+strings.Join(summary.Clients, " ")+" "+strings.Join(summary.IdentitySources, " ")), query) {
			continue
		}
		results = append(results, summary)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].UpdatedAt.Equal(results[j].UpdatedAt) {
			return results[i].ID < results[j].ID
		}
		return results[i].UpdatedAt.After(results[j].UpdatedAt)
	})
	total := len(results)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return clone(append([]Summary{}, results[offset:end]...)), total
}

func (s *Store) Detail(id string, offset, limit int) (Summary, []Record, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return Summary{}, nil, 0, os.ErrNotExist
	}
	rows := make([]Record, 0, len(e.records))
	for _, record := range e.records {
		rows = append(rows, decorate(record, e.tools))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].StartedAt.Equal(rows[j].StartedAt) {
			return rows[i].ID > rows[j].ID
		}
		return rows[i].StartedAt.After(rows[j].StartedAt)
	})
	total := len(rows)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return clone(summarize(e)), clone(rows[offset:end]), total, nil
}

func (s *Store) Record(sessionID, requestID string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[sessionID]
	if e == nil {
		return Record{}, os.ErrNotExist
	}
	r, ok := e.records[requestID]
	if !ok {
		return Record{}, os.ErrNotExist
	}
	return clone(decorate(r, e.tools)), nil
}

func (s *Store) ReadBody(sessionID, requestID, direction string) (io.ReadCloser, error) {
	r, err := s.Record(sessionID, requestID)
	if err != nil {
		return nil, err
	}
	if direction != "request" && direction != "response" {
		return nil, os.ErrNotExist
	}
	if direction == "response" && r.Outcome == "running" {
		return nil, errors.New("response is still being recorded")
	}
	var sealed []byte
	err = s.db.SQL().QueryRow(`SELECT body FROM session_bodies WHERE session_id = ? AND request_id = ? AND direction = ?`, sessionID, requestID, direction).Scan(&sealed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	// The stored bytes are the chunked-encrypted body format (same contexts as
	// the writer used); no outer decryption is needed.
	return &bodyReader{reader: &bytesReader{b: sealed}, aead: s.db.AEAD(), context: sessionID + "/" + requestID + "/" + direction}, nil
}

func (s *Store) Update(id string, name *string, archived *bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return os.ErrNotExist
	}
	meta := clone(e.meta)
	if name != nil {
		if len(strings.TrimSpace(*name)) == 0 || len([]rune(*name)) > 120 || strings.ContainsAny(*name, "\r\n\x00") {
			return errors.New("session name must contain 1–120 characters without line breaks")
		}
		meta.Name = strings.TrimSpace(*name)
		meta.NameInfo = NameInfo{Source: "manual", UpdatedAt: time.Now()}
	}
	if archived != nil {
		meta.Archived = *archived
	}
	if err := s.saveMeta(meta); err != nil {
		return err
	}
	e.meta = meta
	return nil
}

// RecordsSince returns every decoded record started at or after from (or all
// records when from is zero). Used by the usage aggregation endpoint; the
// caller must not hold any session identifiers across the call.
func (s *Store) RecordsSince(from time.Time) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, e := range s.entries {
		for _, r := range e.records {
			if r.StartedAt.Before(from) {
				continue
			}
			out = append(out, clone(decorate(r, e.tools)))
		}
	}
	return out
}
