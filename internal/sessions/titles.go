package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// TextContent extracts only textual blocks, excluding tools, images and hidden
// reasoning. Exact text is used as evidence; prefixes and word similarity aren't.
func TextContent(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var out strings.Builder
	for _, b := range blocks {
		if b.Type == "text" || b.Type == "input_text" || b.Type == "output_text" {
			out.WriteString(b.Text)
		}
	}
	return out.String()
}

func HumanMessages(path string, body []byte) []string {
	if path != "/v1/chat/completions" && path != "/v1/messages" && path != "/v1/responses" {
		return nil
	}
	var p struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &p) != nil {
		return nil
	}
	rows := p.Messages
	if path == "/v1/responses" {
		var text string
		if json.Unmarshal(p.Input, &text) == nil {
			return []string{text}
		}
		if json.Unmarshal(p.Input, &rows) != nil {
			return nil
		}
	}
	var out []string
	for _, m := range rows {
		if m.Role == "user" {
			var plain string
			if json.Unmarshal(m.Content, &plain) != nil {
				var blocks []struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(m.Content, &blocks) != nil {
					return nil
				}
				for _, b := range blocks {
					if b.Type != "text" && b.Type != "input_text" {
						return nil
					}
				}
			}
			if text := TextContent(m.Content); strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

func MessageHashes(messages []string) []string {
	if len(messages) == 0 || len(messages) > 256 {
		return nil
	}
	var out []string
	for _, text := range messages {
		text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
		if text == "" || len(text) > 1<<20 {
			return nil
		}
		h := sha256.Sum256([]byte(text))
		out = append(out, hex.EncodeToString(h[:]))
	}
	return out
}

func messagePrefix(source, history []string) bool {
	if len(source) == 0 || len(source) > len(history) {
		return false
	}
	for i, hash := range source {
		if hash != history[i] {
			return false
		}
	}
	return true
}

func compatibleClient(a, b string) bool {
	return a == "" || b == "" || a == "Unknown" || b == "Unknown" || a == b
}

// ResolveTitleSession matches exact human input in one provider (or any
// platform when provider is empty), without using model, account, IP or
// proximity in time as identity evidence.
func (s *Store) ResolveTitleSession(provider, client string, source []string) (sid, requestID, note string) {
	if len(source) == 0 {
		return "", "", "unmatched"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var chosen Record
	for _, e := range s.entries {
		for _, r := range e.records {
			if r.Purpose == "title" || !r.Request.Complete || !compatibleClient(client, r.Identity.Client) || !messagePrefix(source, r.UserMessages) {
				continue
			}
			if provider != "" && r.Provider != provider {
				continue
			}
			if sid != "" && sid != r.SessionID {
				return "", "", "ambiguous"
			}
			sid = r.SessionID
			if chosen.ID == "" || r.StartedAt.After(chosen.StartedAt) || r.StartedAt.Equal(chosen.StartedAt) && r.ID > chosen.ID {
				chosen = r
			}
		}
	}
	if sid == "" {
		return "", "", "unmatched"
	}
	return sid, chosen.ID, "user_messages"
}

// Headerless clients can issue the title before the main request. Adopt only a
// unique title-only session, so the bodies and token totals never need moving.
func (s *Store) ResolveTitleSeed(provider, client string, history []string, claimant string) (string, string) {
	if len(history) == 0 || !ValidID(claimant) {
		return "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var sid, requestID string
	for _, e := range s.entries {
		if e.seedClaim != "" {
			continue
		}
		onlyTitles := len(e.records) > 0
		for _, r := range e.records {
			if r.Purpose != "title" {
				onlyTitles = false
			}
		}
		if !onlyTitles {
			continue
		}
		for _, r := range e.records {
			if r.Title == nil || r.Title.Association != "unmatched" || !compatibleClient(client, r.Identity.Client) || !messagePrefix(r.Title.SourceMessages, history) {
				continue
			}
			if provider != "" && r.Provider != provider {
				continue
			}
			if sid != "" && sid != r.SessionID {
				return "", ""
			}
			sid, requestID = r.SessionID, r.ID
		}
	}
	if sid != "" {
		s.entries[sid].seedClaim = claimant
	}
	return sid, requestID
}

// NormalizeTitle accepts a short plain line or a JSON object's title field.
// Multiline replies, tools, reasoning and arbitrary JSON are never display names.
func NormalizeTitle(raw string) (string, bool) {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "{") {
		var obj struct {
			Title string `json:"title"`
		}
		if json.Unmarshal([]byte(text), &obj) != nil {
			return "", false
		}
		text = strings.TrimSpace(obj.Title)
	} else if strings.HasPrefix(text, "\"") {
		var plain string
		if json.Unmarshal([]byte(text), &plain) == nil {
			text = strings.TrimSpace(plain)
		}
	}
	if !utf8.ValidString(text) || text == "" || len([]rune(text)) > 120 || strings.HasPrefix(text, "```") {
		return "", false
	}
	for _, r := range text {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return "", false
		}
	}
	return text, true
}

func defaultNameInfo(meta sessionMeta) NameInfo {
	if meta.NameInfo.Source != "" {
		return meta.NameInfo
	}
	source := "id"
	if meta.Name != "" && meta.Name != meta.ID {
		source = "manual"
	}
	return NameInfo{Source: source}
}

func copyTitle(r Record) Record {
	if r.Title != nil {
		value := *r.Title
		r.Title = &value
	}
	return r
}

// applyTitleLocked runs after the response record is durable. The metadata write
// is atomic; a restart reconciles a record left in pending status after a crash.
func (s *Store) applyTitleLocked(e *entry, r *Record) {
	t := r.Title
	if t == nil || r.Outcome == "running" {
		return
	}
	if r.Outcome != "success" || !r.Response.Complete || r.CaptureError != "" {
		t.Status = "failed"
		return
	}
	if t.Error != "" || t.Text == "" {
		t.Status = "invalid_output"
		return
	}
	if t.Association != "session_id" && t.Association != "user_messages" {
		t.Status = "unassociated"
		return
	}
	nameInfo := defaultNameInfo(e.meta)
	if nameInfo.Source == "manual" {
		t.Status = "manual_name_preserved"
		return
	}
	if nameInfo.RequestID == r.ID {
		t.Status = "applied"
		return
	}
	if nameInfo.Source == "model_title" && (r.StartedAt.Before(nameInfo.RequestStartedAt) || r.StartedAt.Equal(nameInfo.RequestStartedAt) && r.ID < nameInfo.RequestID) {
		t.Status = "superseded"
		return
	}
	text, ok := NormalizeTitle(t.Text)
	if !ok {
		t.Status, t.Error = "invalid_output", "invalid_title"
		return
	}
	meta := e.meta
	meta.Name = text
	meta.NameInfo = NameInfo{Source: "model_title", RequestID: r.ID, Detector: t.Detector, UpdatedAt: time.Now(), RequestStartedAt: r.StartedAt}
	if err := s.saveMeta(meta); err != nil {
		t.Status, t.Error = "storage_error", "title_metadata_save_failed"
		return
	}
	e.meta = meta
	t.Status = "applied"
}

func (s *Store) reconcileTitlesLocked(e *entry) error {
	for id, current := range e.records {
		if current.Title == nil {
			continue
		}
		r := copyTitle(current)
		changed := false
		if r.Title.Association == "unmatched" {
			for _, main := range e.records {
				// Provider equality used to scope the match to one platform.
				// With model-global routing the main record's provider is still
				// empty at Begin (the account is chosen later), so an empty
				// side matches any platform within this same session.
				providerOK := main.Provider == "" || r.Provider == "" || main.Provider == r.Provider
				if main.Purpose != "title" && providerOK && main.Request.Complete && compatibleClient(main.Identity.Client, r.Identity.Client) && messagePrefix(r.Title.SourceMessages, main.UserMessages) {
					r.Title.Association, r.Title.LinkedRequestID = "user_messages", main.ID
					changed = true
					break
				}
			}
		}
		if (r.Title.Status == "pending" || r.Title.Status == "unassociated") && r.Outcome != "running" {
			previousStatus, previousError := r.Title.Status, r.Title.Error
			s.applyTitleLocked(e, &r)
			changed = changed || previousStatus != r.Title.Status || previousError != r.Title.Error
		}
		if changed {
			if err := s.saveRecord(r); err != nil {
				return err
			}
			e.records[id] = r
		}
	}
	return nil
}
