// Package accounts persists operator-managed accounts with authenticated
// encryption inside the shared gateway SQLite database. Each account is one
// encrypted row; the whole-file format from earlier versions is migrated by
// MigrateDB.
package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"daydayup/internal/config"
	"daydayup/internal/store"
)

type record struct {
	Account      config.Account `json:"account"`
	APIKey       string         `json:"api_key,omitempty"`
	QuotaFeedKey string         `json:"quota_feed_key,omitempty"`
	Deleted      bool           `json:"deleted,omitempty"`
}

// Store persists managed accounts in the gateway database.
type Store struct {
	db      *store.DB
	records map[string]record
}

// New opens the account store inside the gateway database at dbPath.
func New(dbPath string) (*Store, error) {
	db, err := store.Shared(dbPath)
	if err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	return &Store{db: db, records: map[string]record{}}, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// Load reads stored accounts from the database and merges them over the
// config-declared base accounts (env-backed credentials win for their env
// fields). A missing database row set is valid.
func (s *Store) Load(base []config.Account) ([]config.Account, error) {
	rows, err := s.db.SQL().Query(`SELECT id, record FROM accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("accounts: read: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var sealed []byte
		if err := rows.Scan(&id, &sealed); err != nil {
			return nil, fmt.Errorf("accounts: scan: %w", err)
		}
		plain, err := s.db.Open(sealed, "account:"+id)
		if err != nil {
			return nil, err
		}
		var r record
		if err := json.Unmarshal([]byte(plain), &r); err != nil {
			return nil, fmt.Errorf("accounts: invalid stored account %s", id)
		}
		if _, ok := s.records[r.Account.ID]; ok {
			return nil, errors.New("duplicate stored account")
		}
		s.records[r.Account.ID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("accounts: iterate: %w", err)
	}
	return s.merge(base)
}

// merge overlays stored records over the config base accounts (existing
// behavior preserved).
func (s *Store) merge(base []config.Account) ([]config.Account, error) {
	merged := map[string]config.Account{}
	order := []string{}
	for _, a := range base {
		merged[a.ID] = a
		order = append(order, a.ID)
	}
	ids := make([]string, 0, len(s.records))
	for id := range s.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := s.records[id]
		if r.Deleted {
			delete(merged, id)
			continue
		}
		a := r.Account
		a.Managed = true
		a.APIKey = r.APIKey
		a.QuotaFeedKey = r.QuotaFeedKey
		if a.APIKeyEnv != "" {
			a.APIKey = os.Getenv(a.APIKeyEnv)
		}
		if a.QuotaFeedKeyEnv != "" {
			a.QuotaFeedKey = os.Getenv(a.QuotaFeedKeyEnv)
		}
		if a.RefreshTokenEnv != "" {
			a.RefreshToken = os.Getenv(a.RefreshTokenEnv)
		}
		if a.UIDEnv != "" {
			a.UID = os.Getenv(a.UIDEnv)
		}
		if err := config.NormalizeAccount(&a); err != nil {
			return nil, fmt.Errorf("stored account %s: %w", id, err)
		}
		if _, exists := merged[id]; !exists {
			order = append(order, id)
		}
		merged[id] = a
	}
	out := make([]config.Account, 0, len(merged))
	seen := map[string]bool{}
	for _, id := range order {
		if a, ok := merged[id]; ok {
			identity := a.BaseURL + "\x00" + a.APIKey
			if seen[identity] {
				return nil, errors.New("duplicate stored upstream credential")
			}
			seen[identity] = true
			out = append(out, a)
		}
	}
	return out, nil
}

// SaveAccount stores one managed account (or replaces it). Env-backed
// WorkBuddy tokens keep only the variable reference; the literal must not land
// in the encrypted store as a second copy.
func (s *Store) SaveAccount(a config.Account) error {
	r := record{Account: a}
	if a.APIKeyEnv == "" {
		r.APIKey = a.APIKey
	}
	if a.QuotaFeedKeyEnv == "" {
		r.QuotaFeedKey = a.QuotaFeedKey
	}
	if a.RefreshTokenEnv != "" {
		r.Account.RefreshToken = ""
	}
	if a.UIDEnv != "" {
		r.Account.UID = ""
	}
	return s.write(a.ID, r)
}

// Delete tombstones the stored account.
func (s *Store) Delete(id string) error {
	return s.write(id, record{Account: config.Account{ID: id}, Deleted: true})
}

func (s *Store) write(id string, r record) error {
	plain, err := json.Marshal(r)
	if err != nil {
		return err
	}
	sealed, err := s.db.Seal(string(plain), "account:"+id)
	if err != nil {
		return err
	}
	if _, err := s.db.SQL().Exec(`INSERT INTO accounts(id, record) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET record = excluded.record`, id, sealed); err != nil {
		return fmt.Errorf("accounts: save %s: %w", id, err)
	}
	s.records[id] = r
	return nil
}
