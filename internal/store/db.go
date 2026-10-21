// Package store is the single local persistence boundary for the gateway:
// accounts, pool state, session archives, redaction rules and agent bindings
// all live in one SQLite database file. Sensitive values (account credentials,
// session bodies and metadata, redaction reverse mappings) stay encrypted with
// AES-256-GCM at the application layer, so the database file itself never
// contains plaintext secrets.
//
// The encryption key comes from the GATEWAY_DB_KEY environment variable
// (32 bytes, standard base64) or from an auto-generated key file
// <db_file>.key with mode 0600. Legacy per-domain stores (accounts.enc,
// state.json, the sessions directory, redaction.map.enc, agents.json) are
// migrated into this database on first open; see the per-domain Migrate
// functions.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

// DefaultKeyEnv is the environment variable holding the database encryption
// key (32 bytes, standard base64). When unset, a random key is generated into
// <db_file>.key on first open.
const DefaultKeyEnv = "GATEWAY_DB_KEY"

// DB wraps the SQLite handle plus the application-layer AEAD used to encrypt
// sensitive columns. It is safe for concurrent use by one process.
type DB struct {
	mu       sync.Mutex
	sql      *sql.DB
	path     string
	keyEnv   string
	aead     cipher.AEAD
	cacheKey string // non-empty when returned by Shared/SharedKey
}

// sharedDBs caches one process-wide handle per database path. Production
// startup constructs every store (accounts, sessions, pool, redaction,
// agents) through Shared so a boot opens the (possibly multi-gigabyte)
// database exactly once instead of six independent handles — and so no
// handle is ever closed during boot, which would otherwise trigger SQLite's
// blocking WAL checkpoint on last-connection close.
var (
	sharedMu  sync.Mutex
	sharedDBs = map[string]*DB{}
)

// Shared returns the process-wide handle for path, opening it once and
// caching it for the lifetime of the process. The returned handle must not
// be closed by production code; Close evicts the cache entry.
func Shared(path string) (*DB, error) {
	return SharedKey(path, DefaultKeyEnv)
}

// SharedKey is Shared with an explicit key environment variable name.
func SharedKey(path, keyEnv string) (*DB, error) {
	if keyEnv == "" {
		keyEnv = DefaultKeyEnv
	}
	k := path + "\x00" + keyEnv
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if db, ok := sharedDBs[k]; ok {
		return db, nil
	}
	db, err := OpenKey(path, keyEnv)
	if err != nil {
		return nil, err
	}
	db.cacheKey = k
	sharedDBs[k] = db
	return db, nil
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema. The returned handle is the single persistence point for every store.
// Tests use Open for isolated handles; production startup paths use Shared.
func Open(path string) (*DB, error) {
	return OpenKey(path, DefaultKeyEnv)
}

// OpenKey is Open with an explicit key environment variable name.
func OpenKey(path string, keyEnv string) (*DB, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if keyEnv == "" {
		keyEnv = DefaultKeyEnv
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("store: create database directory: %w", err)
	}
	// An existing zero-length file is treated as corruption (fails closed)
	// rather than silently initialized as a fresh database.
	if info, statErr := os.Stat(path); statErr == nil && info.Size() == 0 {
		return nil, errors.New("store: database file exists but is empty")
	}
	// The path is percent-encoded: characters like "#" (Go test subtests) or
	// spaces would otherwise be parsed as URI fragment/query by the file:
	// DSN and silently point every store at one truncated database.
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	sqlDB.SetMaxOpenConns(1) // single-writer process; serialized access avoids SQLITE_BUSY storms
	aead, err := loadAEAD(path, keyEnv)
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	db := &DB{sql: sqlDB, path: path, keyEnv: keyEnv, aead: aead}
	if err := db.createSchema(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	// The database holds personal gateway state; keep the file private even
	// when the process umask is permissive.
	secureFile(path)
	return db, nil
}

func secureFile(path string) {
	if err := os.Chmod(path, 0600); err == nil {
		for _, suffix := range []string{"-wal", "-shm"} {
			wal := path + suffix
			if _, err := os.Stat(wal); err == nil {
				_ = os.Chmod(wal, 0600)
			}
		}
	}
}

// Close releases the database handle. A handle returned by Shared is evicted
// from the process cache first, so a later Shared call reopens the database.
func (d *DB) Close() error {
	if d.cacheKey != "" {
		sharedMu.Lock()
		if sharedDBs[d.cacheKey] == d {
			delete(sharedDBs, d.cacheKey)
		}
		sharedMu.Unlock()
	}
	return d.sql.Close()
}

// Path returns the database file path.
func (d *DB) Path() string { return d.path }

// SQL exposes the underlying handle for domain queries.
func (d *DB) SQL() *sql.DB { return d.sql }

// AEAD exposes the shared application-layer cipher for domain code that needs
// raw authenticated chunking (session body streams).
func (d *DB) AEAD() cipher.AEAD { return d.aead }

// Seal encrypts plain with a nonce and authenticates it against context.
// The output is nonce || ciphertext (with tag). Context is not secret; it
// binds a value to its logical location so swapped rows fail authentication.
func (d *DB) Seal(plain, context string) ([]byte, error) {
	nonce := make([]byte, d.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return d.aead.Seal(nonce, nonce, []byte(plain), []byte(context)), nil
}

// Open decrypts a value produced by Seal.
func (d *DB) Open(sealed []byte, context string) (string, error) {
	if len(sealed) < d.aead.NonceSize()+d.aead.Overhead() {
		return "", errors.New("store: truncated encrypted value")
	}
	plain, err := d.aead.Open(nil, sealed[:d.aead.NonceSize()], sealed[d.aead.NonceSize():], []byte(context))
	if err != nil {
		return "", errors.New("store: encrypted value authentication failed")
	}
	return string(plain), nil
}

// SealBytes encrypts a raw byte payload (used for session body chunks, whose
// format already includes per-chunk authentication; the outer seal adds a
// location binding).
func (d *DB) SealBytes(plain []byte, context string) ([]byte, error) {
	nonce := make([]byte, d.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return d.aead.Seal(nonce, nonce, plain, []byte(context)), nil
}

// OpenBytes decrypts a value produced by SealBytes.
func (d *DB) OpenBytes(sealed []byte, context string) ([]byte, error) {
	if len(sealed) < d.aead.NonceSize()+d.aead.Overhead() {
		return nil, errors.New("store: truncated encrypted value")
	}
	plain, err := d.aead.Open(nil, sealed[:d.aead.NonceSize()], sealed[d.aead.NonceSize():], []byte(context))
	if err != nil {
		return nil, errors.New("store: encrypted value authentication failed")
	}
	return plain, nil
}

func loadAEAD(path, keyEnv string) (cipher.AEAD, error) {
	var key []byte
	var err error
	if value := os.Getenv(keyEnv); value != "" {
		key, err = base64.StdEncoding.DecodeString(value)
		if err != nil || len(key) != 32 {
			return nil, errors.New("store: database key must be 32 bytes encoded as standard base64")
		}
	} else {
		keyPath := path + ".key"
		key, err = os.ReadFile(keyPath)
		if os.IsNotExist(err) {
			// Never create a fresh random key over existing data: a lost key
			// must fail loudly instead of silently making the archive
			// unrecoverable.
			if info, statErr := os.Stat(path); statErr == nil && info.Size() > 0 {
				return nil, errors.New("store: database exists but its key file is missing")
			}
			key = make([]byte, 32)
			if _, err = rand.Read(key); err != nil {
				return nil, err
			}
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return nil, err
			}
			f, e := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				return nil, e
			}
			_, err = f.Write(key)
			if err == nil {
				err = f.Sync()
			}
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("store: database key must be 32 bytes (GATEWAY_DB_KEY or <db_file>.key)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (d *DB) createSchema() error {
	const schema = `
CREATE TABLE IF NOT EXISTS store_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS accounts (
	id     TEXT PRIMARY KEY,
	record BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS pool_state (
	id    TEXT PRIMARY KEY,
	state TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions_meta (
	id  TEXT PRIMARY KEY,
	meta BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS session_records (
	session_id TEXT NOT NULL,
	request_id TEXT NOT NULL,
	record     BLOB NOT NULL,
	PRIMARY KEY (session_id, request_id)
);
CREATE TABLE IF NOT EXISTS session_bodies (
	session_id TEXT NOT NULL,
	request_id TEXT NOT NULL,
	direction  TEXT NOT NULL,
	body       BLOB NOT NULL,
	PRIMARY KEY (session_id, request_id, direction)
);
CREATE TABLE IF NOT EXISTS session_tools (
	session_id TEXT NOT NULL,
	tool_id    TEXT NOT NULL,
	event      BLOB NOT NULL,
	PRIMARY KEY (session_id, tool_id)
);
CREATE TABLE IF NOT EXISTS redact_rules (
	scope TEXT PRIMARY KEY,
	rules BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS agents_bindings (
	agent_id TEXT PRIMARY KEY,
	binding  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agents_profiles (
	name    TEXT PRIMARY KEY,
	profile TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_records_session ON session_records(session_id);
CREATE INDEX IF NOT EXISTS idx_tools_session ON session_tools(session_id);
`
	if _, err := d.sql.Exec(schema); err != nil {
		return fmt.Errorf("store: create schema: %w", err)
	}
	return nil
}

// Meta returns the stored metadata value for key ("" when absent).
func (d *DB) Meta(key string) (string, error) {
	var value string
	err := d.sql.QueryRow(`SELECT value FROM store_meta WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// SetMeta stores a metadata value.
func (d *DB) SetMeta(key, value string) error {
	_, err := d.sql.Exec(`INSERT INTO store_meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Migrated reports whether a migration marker is present.
func (d *DB) Migrated(key string) (bool, error) {
	v, err := d.Meta(key)
	return v != "", err
}

// MarkMigrated records a completed migration marker.
func (d *DB) MarkMigrated(key string) error { return d.SetMeta(key, "1") }
