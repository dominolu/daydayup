package sessions

import "os"

// PreferredAccount returns the account the session pinned for a model. Routing
// is model-global (cross-platform), so the pin is keyed by model id: repeating
// the same model reuses the same account on whichever platform served it last.
func (s *Store) PreferredAccount(id, model string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil {
		return e.meta.PreferredAccounts[model]
	}
	return ""
}

func (s *Store) BindAccount(id, model, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return os.ErrNotExist
	}
	if e.meta.PreferredAccounts[model] == accountID {
		return nil
	}
	meta := clone(e.meta)
	meta.PreferredAccounts[model] = accountID
	if err := s.saveMeta(meta); err != nil {
		return err
	}
	e.meta = meta
	return nil
}

// Response IDs are scoped by upstream platform and resolved only when
// unambiguous. An empty provider searches every platform (global routing:
// a continuation may have been issued by any platform's account).
func (s *Store) ResolveResponse(provider, responseID string) (string, string, bool) {
	if responseID == "" {
		return "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var sessionID, accountID string
	for _, e := range s.entries {
		for _, r := range e.records {
			if r.ResponseID != responseID || r.AccountID == "" {
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

// LockRoute serializes selection and binding, while inference remains concurrent.
func (s *Store) LockRoute(id string) func() {
	s.mu.Lock()
	e := s.entries[id]
	s.mu.Unlock()
	if e == nil {
		return func() {}
	}
	e.routeMu.Lock()
	return e.routeMu.Unlock
}

func hasField(fields []string, key string) bool {
	for _, field := range fields {
		if field == key {
			return true
		}
	}
	return false
}
func mergeFields(target, fields []string) []string {
	for _, field := range fields {
		if !hasField(target, field) {
			target = append(target, field)
		}
	}
	return target
}
