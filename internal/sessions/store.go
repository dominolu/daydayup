// Package sessions archives authenticated gateway exchanges with encrypted
// streaming bodies. It stores supplied history, without generating or replaying it.
//
// Durable storage is the shared gateway SQLite database: one row per session
// metadata, per request record, per body and per tool event, all encrypted at
// the application layer. While a request streams, its body chunks are written
// to a transient temp file so a crash never loses the partial body; finished
// bodies are imported into the database and the temp file is removed.
package sessions

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"safuno/internal/store"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func ValidID(id string) bool { return idPattern.MatchString(id) }

type entry struct {
	seedClaim string
	routeMu   sync.Mutex
	meta      sessionMeta
	records   map[string]Record
	tools     map[string]ToolEvent
}

type Store struct {
	mu            sync.Mutex
	dbPath        string
	db            *store.DB
	dbOnce        sync.Once
	dbErr         error
	tmpDir        string
	entries       map[string]*entry
	retentionDays int                // body retention window (0 = off)
	maxBodyMB     int                // total body capacity cap (0 = off)
	pruned        map[[2]string]bool // request owners whose bodies were already pruned
	bodyFiles     bool               // archive bodies on disk with a DB index only
}

// Open opens the session archive inside the gateway database at dbPath with
// its own freshly opened handle (each call revalidates the database key and
// reloads the archive). Tests and legacy callers use Open; the production
// gateway uses OpenWithRetention, which shares the process-wide handle so a
// boot never opens the multi-gigabyte database more than once.
// keyEnv is retained for the legacy migration path and ignored otherwise; the
// database key comes from store.Open.
func Open(dbPath, keyEnv string) (*Store, error) {
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return openWith(dbPath, db, Retention{})
}

// OpenWithRetention opens the session archive inside the gateway database at
// dbPath using the process-wide shared handle, and applies the configured
// body retention policy after loading. keyEnv is ignored; the database key
// comes from store.Open.
func OpenWithRetention(dbPath string, retention Retention) (*Store, error) {
	db, err := store.Shared(dbPath)
	if err != nil {
		return nil, err
	}
	return openWith(dbPath, db, retention)
}

func openWith(dbPath string, db *store.DB, retention Retention) (*Store, error) {
	s := &Store{dbPath: dbPath, db: db, tmpDir: filepath.Join(filepath.Dir(dbPath), "tmp"), entries: map[string]*entry{}, retentionDays: retention.Days, maxBodyMB: retention.MaxBodyMB, pruned: map[[2]string]bool{}, bodyFiles: retention.BodyFilesEnabled()}
	if err := os.MkdirAll(s.tmpDir, 0700); err != nil {
		return nil, err
	}
	if s.bodyFiles {
		if err := os.MkdirAll(s.filesRoot(), 0700); err != nil {
			return nil, err
		}
	}
	if err := s.loadAll(); err != nil {
		return nil, err
	}
	// Retention and file-index maintenance are applied by the gateway in the
	// background after boot (a large first cut over thousands of bodies takes
	// tens of seconds; it must not delay startup).
	return s, nil
}

// tmpPath returns the transient body file path for a streaming request.
// ValidID guarantees the ids contain no path separators.
func (s *Store) tmpPath(sessionID, requestID, direction string) string {
	return filepath.Join(s.tmpDir, sessionID+"."+requestID+"."+direction)
}

func (s *Store) loadAll() error {
	// Session metadata.
	rows, err := s.db.SQL().Query(`SELECT id, meta FROM sessions_meta`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type metaRow struct {
		id   string
		meta sessionMeta
	}
	var metas []metaRow
	for rows.Next() {
		var id string
		var sealed []byte
		if err := rows.Scan(&id, &sealed); err != nil {
			return err
		}
		plain, err := s.db.Open(sealed, "smeta:"+id)
		if err != nil {
			return err
		}
		var meta sessionMeta
		if err := decodeMeta(plain, &meta); err != nil {
			return err
		}
		if !ValidID(meta.ID) || meta.ID != id {
			return errors.New("invalid session identity")
		}
		metas = append(metas, metaRow{id: id, meta: meta})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Records.
	rows, err = s.db.SQL().Query(`SELECT session_id, request_id, record FROM session_records`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type recordRow struct {
		session string
		record  Record
	}
	var recs []recordRow
	for rows.Next() {
		var sessionID, requestID string
		var sealed []byte
		if err := rows.Scan(&sessionID, &requestID, &sealed); err != nil {
			return err
		}
		plain, err := s.db.Open(sealed, "srec:"+sessionID+"/"+requestID)
		if err != nil {
			return err
		}
		var record Record
		if err := decodeRecord(plain, &record); err != nil {
			return err
		}
		if record.ID != requestID || record.SessionID != sessionID {
			return errors.New("session record identity mismatch")
		}
		recs = append(recs, recordRow{session: sessionID, record: record})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Tools.
	tools, err := s.loadAllTools()
	if err != nil {
		return err
	}
	// Assemble entries in a stable order (meta first, then records).
	for _, mr := range metas {
		e := &entry{meta: mr.meta, records: map[string]Record{}, tools: map[string]ToolEvent{}}
		if e.meta.PreferredAccounts == nil {
			e.meta.PreferredAccounts = map[string]string{}
		}
		e.meta.NameInfo = defaultNameInfo(e.meta)
		for _, rr := range recs {
			if rr.session != mr.id {
				continue
			}
			if rr.record.Outcome == "running" {
				rr.record.Outcome = "interrupted"
				rr.record.Response.Complete = false
				rr.record.CaptureError = "gateway restarted before this request completed"
				if err := s.saveRecord(rr.record); err != nil {
					return err
				}
			}
			// Rebuild missing legacy fingerprints from authenticated requests.
			// Reads the stored body directly: the entry is not registered in
			// s.entries until this loop finishes, so Record/ReadBody cannot
			// be used here.
			if rr.record.Title == nil && len(rr.record.UserMessages) == 0 && rr.record.Request.Complete && rr.record.Request.Bytes <= 1<<20 {
				sealed, err := s.storedBodyBytes(mr.id, rr.record.ID, "request")
				if err == nil {
					reader := &bodyReader{reader: &bytesReader{b: sealed}, aead: s.db.AEAD(), context: mr.id + "/" + rr.record.ID + "/request"}
					b, readErr := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
					if readErr == nil && len(b) <= 1<<20 {
						rr.record.UserMessages = MessageHashes(HumanMessages(rr.record.Path, b))
					}
				}
			}
			e.records[rr.record.ID] = rr.record
		}
		for toolID, tool := range tools[mr.id] {
			e.tools[toolID] = tool
		}
		s.entries[mr.id] = e
		if err := s.reconcileTitlesLocked(e); err != nil {
			return err
		}
	}
	// Import leftover transient bodies from a crashed run as interrupted data.
	return s.importLeftovers()
}

func (s *Store) saveRecord(r Record) error {
	plain, err := encodeRecord(r)
	if err != nil {
		return err
	}
	sealed, err := s.db.Seal(plain, "srec:"+r.SessionID+"/"+r.ID)
	if err != nil {
		return err
	}
	_, err = s.db.SQL().Exec(`INSERT INTO session_records(session_id, request_id, record) VALUES(?, ?, ?) ON CONFLICT(session_id, request_id) DO UPDATE SET record = excluded.record`, r.SessionID, r.ID, sealed)
	return err
}

func (s *Store) saveMeta(meta sessionMeta) error {
	plain, err := encodeMeta(meta)
	if err != nil {
		return err
	}
	sealed, err := s.db.Seal(plain, "smeta:"+meta.ID)
	if err != nil {
		return err
	}
	_, err = s.db.SQL().Exec(`INSERT INTO sessions_meta(id, meta) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET meta = excluded.meta`, meta.ID, sealed)
	return err
}

// importLeftovers moves transient body files left by a crashed process into
// durable storage (renamed into the session archive in file-backed mode, or
// imported as BLOBs in database mode), then removes the source files.
func (s *Store) importLeftovers() error {
	files, err := os.ReadDir(s.tmpDir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		// <session>.<request>.<direction>
		i := strings.LastIndex(name, ".")
		if i <= 0 {
			continue
		}
		j := strings.LastIndex(name[:i], ".")
		if j <= 0 {
			continue
		}
		sessionID, requestID, direction := name[:j], name[j+1:i], name[i+1:]
		if !ValidID(sessionID) || !ValidID(requestID) || (direction != "request" && direction != "response") {
			continue
		}
		path := filepath.Join(s.tmpDir, name)
		if s.bodyFiles {
			dst := s.bodyPath(sessionID, requestID, direction)
			if dir := filepath.Dir(dst); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					return err
				}
			}
			if err := os.Rename(path, dst); err != nil {
				return err
			}
			if err := s.ensureBodyIndex(sessionID, requestID, direction); err != nil {
				return err
			}
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, err := s.db.SQL().Exec(`INSERT INTO session_bodies(session_id, request_id, direction, body) VALUES(?, ?, ?, ?) ON CONFLICT(session_id, request_id, direction) DO NOTHING`, sessionID, requestID, direction, b); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// readBodyBytes returns the decrypted body bytes of a stored record.
func (s *Store) readBodyBytes(sessionID, requestID, direction string) ([]byte, error) {
	r, err := s.ReadBody(sessionID, requestID, direction)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func encodeMeta(m sessionMeta) (string, error) {
	b, err := json.Marshal(m)
	return string(b), err
}

func decodeMeta(s string, m *sessionMeta) error {
	return json.Unmarshal([]byte(s), m)
}

func encodeRecord(r Record) (string, error) {
	b, err := json.Marshal(r)
	return string(b), err
}

func decodeRecord(s string, r *Record) error {
	return json.Unmarshal([]byte(s), r)
}
