package sessions

// An opaque server routing token belongs to the account that issued it, even
// when a later stateless request changed the session's preferred account.
// provider == "" searches every platform (global routing).
func (s *Store) ResolveTurnState(provider, hash string) (sessionID, accountID string, ok bool) {
	if hash == "" {
		return "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		for _, r := range e.records {
			if r.TurnStateHash != hash || r.AccountID == "" || r.Outcome != "success" || !r.Response.Complete || r.CaptureError != "" {
				continue
			}
			if provider != "" && r.Provider != provider {
				continue
			}
			if sessionID != "" && (sessionID != r.SessionID || accountID != r.AccountID) {
				return "", "", false
			}
			sessionID, accountID = r.SessionID, r.AccountID
		}
	}
	return sessionID, accountID, sessionID != ""
}

// ResolveToolContinuation requires the original request history and every tool
// result ID to match a complete successful exchange in the same protocol (and,
// when provider is non-empty, the same platform). Ambiguous matches are
// deliberately left as separate sessions.
func (s *Store) ResolveToolContinuation(provider, path, history string, ids []string) (sessionID, requestID, note string) {
	if history == "" || len(ids) == 0 {
		return "", "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched Record
	for _, e := range s.entries {
		for _, r := range e.records {
			if r.Path != path || r.HistoryHash != history || r.Outcome != "success" || !r.Response.Complete || r.CaptureError != "" {
				continue
			}
			if provider != "" && r.Provider != provider {
				continue
			}
			all := true
			for _, id := range ids {
				if !hasField(r.ToolCallIDs, id) {
					all = false
					break
				}
			}
			if !all {
				continue
			}
			if sessionID != "" && sessionID != r.SessionID {
				return "", "", "ambiguous_tool_continuation"
			}
			sessionID = r.SessionID
			if matched.ID == "" || r.FinishedAt.After(matched.FinishedAt) || r.FinishedAt.Equal(matched.FinishedAt) && r.ID > matched.ID {
				matched = r
			}
		}
	}
	if sessionID != "" {
		return sessionID, matched.ID, ""
	}
	return "", "", "unmatched_tool_continuation"
}
