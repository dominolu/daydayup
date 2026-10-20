package redact

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// tokenAlphabet is vowel-free so placeholders never spell words. 6 characters
// yield ~2.4e8 distinct tokens per label, enough for personal-scale mapping.
const tokenAlphabet = "bcdfghjkmnpqrstvwxz"

const tokenSuffixLen = 6

type tokenEntry struct {
	label    string
	original string
	touched  time.Time
}

// TokenManager issues stable placeholders within one trust scope. The same
// (label, original) always maps to the same token so multi-turn context and
// session evidence hashes stay stable; originals are never exposed outside
// the manager.
type TokenManager struct {
	scope      string
	ttl        time.Duration
	maxEntries int
	maxToken   int
	store      *Store
	now        func() time.Time

	mu      sync.Mutex
	forward map[string]string // key(scope, label, original) -> token
	reverse map[string]tokenEntry
	suffix  map[string]string // key(scope, suffix) -> token
	dirty   bool
}

func newTokenManager(cfg Config, store *Store) *TokenManager {
	return &TokenManager{
		scope:      cfg.ScopeValue,
		ttl:        time.Duration(cfg.TokenTTLSeconds) * time.Second,
		maxEntries: cfg.MaxMappings,
		maxToken:   cfg.MaxTokenLength,
		store:      store,
		now:        time.Now,
		forward:    map[string]string{},
		reverse:    map[string]tokenEntry{},
		suffix:     map[string]string{},
	}
}

// load rebuilds in-memory indexes from the durable store. The store holds
// reverse entries keyed "scope\x00token".
func (m *TokenManager) load() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := m.store.Load(m.scope)
	if err != nil {
		return err
	}
	for token, entry := range entries {
		if entry.touched.IsZero() {
			entry.touched = m.now()
		} // Legacy stores have no timestamp.
		m.reverse[token] = entry
		m.forward[key(m.scope, entry.label, entry.original)] = token
		if idx := strings.LastIndexByte(token, '_'); idx >= 0 {
			suffix := token[idx+1 : len(token)-2]
			m.suffix[key(m.scope, suffix)] = token
		}
	}
	return nil
}

// TokenFor returns the stable placeholder for (label, original), issuing a new
// token on first sight. The token is remembered in the durable store.
func (m *TokenManager) TokenFor(label, original string) (string, error) {
	if len(original) > m.maxToken {
		return "", fmt.Errorf("redact: matched value exceeds max_token_length")
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	fk := key(m.scope, label, original)
	if token, ok := m.forward[fk]; ok {
		if entry, ok := m.reverse[token]; ok {
			entry.touched = now
			m.reverse[token] = entry
			m.dirty = true
			return token, nil
		}
		delete(m.forward, fk)
	}
	if len(m.reverse) >= m.maxEntries {
		return "", fmt.Errorf("redact: mapping store is full")
	}
	token, err := m.issueLocked(label)
	if err != nil {
		return "", err
	}
	m.reverse[token] = tokenEntry{label: label, original: original, touched: now}
	m.forward[fk] = token
	if idx := strings.LastIndexByte(token, '_'); idx >= 0 {
		m.suffix[key(m.scope, token[idx+1:len(token)-2])] = token
	}
	m.dirty = true
	return token, nil
}

// issueLocked generates a fresh token whose suffix is not already bound.
func (m *TokenManager) issueLocked(label string) (string, error) {
	for attempt := 0; attempt < 64; attempt++ {
		suffix, err := randomSuffix()
		if err != nil {
			return "", err
		}
		token := "{{" + label + "_" + suffix + "}}"
		_, suffixUsed := m.suffix[key(m.scope, suffix)]
		if _, exists := m.reverse[token]; !exists && !suffixUsed {
			return token, nil
		}
	}
	return "", fmt.Errorf("redact: unable to allocate a fresh placeholder")
}

// Resolve finds the original for a placeholder token, with label-normalized
// suffix fallback: {{EMAIL_ab}} restores even if the model echoes
// {{email_ab}} or {{e_mail_ab}}.
func (m *TokenManager) Resolve(token string) (label, original string, ok bool) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	if entry, found := m.reverse[token]; found {
		entry.touched = now
		m.reverse[token] = entry
		m.dirty = true
		return entry.label, entry.original, true
	}
	// Suffix fallback with label normalization (underscores removed,
	// case-insensitive) so a model echoing {{email_ab}} still restores.
	idx := strings.LastIndexByte(token, '_')
	if idx <= 2 || len(token) < idx+3 {
		return "", "", false
	}
	suffix := token[idx+1 : len(token)-2]
	if suffixToken, found := m.suffix[key(m.scope, suffix)]; found {
		if entry, found := m.reverse[suffixToken]; found {
			want := strings.ReplaceAll(token[2:idx], "_", "")
			have := strings.ReplaceAll(entry.label, "_", "")
			if strings.EqualFold(want, have) {
				entry.touched = now
				m.reverse[suffixToken] = entry
				m.dirty = true
				return entry.label, entry.original, true
			}
		}
	}
	return "", "", false
}

// pruneLocked drops expired entries. Live entries may still be needed by an
// in-flight response and are never evicted just to make room.
func (m *TokenManager) pruneLocked(now time.Time) {
	for token, entry := range m.reverse {
		if now.Sub(entry.touched) > m.ttl {
			m.dirty = true
			delete(m.reverse, token)
			delete(m.forward, key(m.scope, entry.label, entry.original))
			if idx := strings.LastIndexByte(token, '_'); idx >= 0 {
				delete(m.suffix, key(m.scope, token[idx+1:len(token)-2]))
			}
		}
	}
}

// Flush persists new mappings to the encrypted store when dirty.
func (m *TokenManager) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.dirty {
		return nil
	}
	entries := make(map[string]tokenEntry, len(m.reverse))
	for token, entry := range m.reverse {
		entries[token] = entry
	}
	if err := m.store.Save(m.scope, entries); err != nil {
		return err
	}
	m.dirty = false
	return nil
}

// Size reports the current number of stored mappings.
func (m *TokenManager) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reverse)
}

func randomSuffix() (string, error) {
	var suffix strings.Builder
	for i := 0; i < tokenSuffixLen; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(tokenAlphabet))))
		if err != nil {
			return "", err
		}
		suffix.WriteByte(tokenAlphabet[n.Int64()])
	}
	return suffix.String(), nil
}

func key(parts ...string) string {
	return strings.Join(parts, "\x00")
}
