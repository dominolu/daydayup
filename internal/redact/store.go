package redact

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"safuno/internal/store"
)

// Store persists the placeholder reverse index (token -> label+original)
// inside the shared gateway database. Each scope is one encrypted row
// (AES-256-GCM). keyEnv is retained for legacy migration only; the database
// key comes from store.Open.
type Store struct {
	path   string
	keyEnv string // deprecated: legacy migration key source
	mu     sync.Mutex
	db     *store.DB
	dbOnce sync.Once
	dbErr  error
}

type storeEntry struct {
	Label    string    `json:"label"`
	Original string    `json:"original"`
	Touched  time.Time `json:"touched,omitempty"`
}

func OpenStore(path, keyEnv string) *Store {
	return &Store{path: path, keyEnv: keyEnv}
}

func (s *Store) open() (*store.DB, error) {
	s.dbOnce.Do(func() {
		s.db, s.dbErr = store.Shared(s.path)
	})
	return s.db, s.dbErr
}

// Load returns the reverse entries for one scope. A missing or empty store is
// valid; a decrypt failure is fatal.
func (s *Store) Load(scope string) (map[string]tokenEntry, error) {
	db, err := s.open()
	if err != nil {
		return nil, fmt.Errorf("redact: %w", err)
	}
	out := map[string]tokenEntry{}
	var sealed []byte
	err = db.SQL().QueryRow(`SELECT rules FROM redact_rules WHERE scope = ?`, scope).Scan(&sealed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("redact: read mapping store: %w", err)
	}
	plain, err := db.Open(sealed, "redact:"+scope)
	if err != nil {
		return nil, fmt.Errorf("redact: decrypt mapping store: %w", err)
	}
	var data struct {
		Version int                              `json:"version"`
		Entries map[string]storeEntry            `json:"entries"`
		Scopes  map[string]map[string]storeEntry `json:"scopes,omitempty"` // legacy shape
	}
	if err := json.Unmarshal([]byte(plain), &data); err != nil {
		return nil, fmt.Errorf("redact: invalid mapping store: %w", err)
	}
	if data.Version != 1 {
		return nil, fmt.Errorf("redact: unsupported mapping store version %d", data.Version)
	}
	entries := data.Entries
	if entries == nil && data.Scopes != nil {
		entries = data.Scopes[scope]
	}
	for token, entry := range entries {
		if !validLabel(entry.Label) || placeholderRE.FindString(token) != token || !strings.HasPrefix(token, "{{"+entry.Label+"_") {
			return nil, errors.New("redact: invalid mapping store entry")
		}
		out[token] = tokenEntry{label: entry.Label, original: entry.Original, touched: entry.Touched}
	}
	return out, nil
}

// Save atomically writes the reverse index for one scope (a single encrypted
// row; other scopes are independent rows and never touched).
func (s *Store) Save(scope string, entries map[string]tokenEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.open()
	if err != nil {
		return fmt.Errorf("redact: %w", err)
	}
	next := map[string]storeEntry{}
	for token, entry := range entries {
		next[token] = storeEntry{Label: entry.label, Original: entry.original, Touched: entry.touched}
	}
	payload, err := json.Marshal(struct {
		Version int                   `json:"version"`
		Entries map[string]storeEntry `json:"entries"`
	}{1, next})
	if err != nil {
		return err
	}
	sealed, err := db.Seal(string(payload), "redact:"+scope)
	if err != nil {
		return err
	}
	if _, err := db.SQL().Exec(`INSERT INTO redact_rules(scope, rules) VALUES(?, ?) ON CONFLICT(scope) DO UPDATE SET rules = excluded.rules`, scope, sealed); err != nil {
		return fmt.Errorf("redact: save mapping store: %w", err)
	}
	return nil
}
