package sessions

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"safuno/internal/store"
)

// MigrateBodiesToFiles exports body BLOBs from the database into the
// per-session archive files (one encrypted file per request direction),
// leaving empty-column index rows behind — the file-backed storage mode.
// The database file is snapshot-copied first so the export can always be
// redone from the backup. Idempotent per marker; runs in the background
// after boot so a large export never delays startup.
func MigrateBodiesToFiles(db *store.DB, dbPath string) error {
	if dbPath == "" {
		return nil
	}
	done, err := db.Migrated("bodies_files_v1")
	if err != nil || done {
		return err
	}
	backup := dbPath + ".bak-bodies-" + time.Now().Format("20060102-150405")
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		if err := copyFile(dbPath, backup); err != nil {
			return fmt.Errorf("bodies migration: backup: %w", err)
		}
	}
	root := bodyFilesRoot(dbPath)
	for {
		rows, err := db.SQL().Query(`SELECT session_id, request_id, direction, body FROM session_bodies WHERE length(body) > 0 LIMIT 500`)
		if err != nil {
			return err
		}
		type item struct {
			sid, rid, dir string
			body          []byte
		}
		var batch []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.sid, &it.rid, &it.dir, &it.body); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, it)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		tx, err := db.SQL().Begin()
		if err != nil {
			return err
		}
		for _, it := range batch {
			if err := writeBodyFile(root, it.sid, it.rid, it.dir, it.body); err != nil {
				tx.Rollback()
				return fmt.Errorf("bodies migration: %s/%s/%s: %w", it.sid, it.rid, it.dir, err)
			}
			if _, err := tx.Exec(`UPDATE session_bodies SET body = ? WHERE session_id = ? AND request_id = ? AND direction = ?`, "", it.sid, it.rid, it.dir); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return db.MarkMigrated("bodies_files_v1")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// MigrateDB imports a legacy directory-based session archive (data/sessions/
// with per-session encrypted files) into the shared database. Records, bodies
// and tool events are re-encrypted under the current database key; the legacy
// directory is never modified. Idempotent per marker.
func MigrateDB(db *store.DB, legacyDir, legacyKeyEnv string) error {
	if legacyDir == "" {
		return nil
	}
	done, err := db.Migrated("sessions_v1")
	if err != nil || done {
		return err
	}
	if _, err := os.Stat(legacyDir); os.IsNotExist(err) {
		return db.MarkMigrated("sessions_v1")
	}
	oldAEAD, err := legacySessionsAEAD(legacyDir, legacyKeyEnv)
	if err != nil {
		return fmt.Errorf("sessions migration: %w", err)
	}
	newAEAD := db.AEAD()
	dirs, err := os.ReadDir(legacyDir)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if !d.IsDir() || !strings.HasPrefix(d.Name(), "session-") {
			continue
		}
		dir := filepath.Join(legacyDir, d.Name())
		var meta sessionMeta
		if err := legacyOpenJSON(oldAEAD, filepath.Join(dir, "session.meta"), d.Name(), &meta); err != nil {
			return fmt.Errorf("sessions migration: %s: %w", d.Name(), err)
		}
		if !ValidID(meta.ID) || sessionDir(meta.ID) != d.Name() {
			return errors.New("sessions migration: invalid session identity")
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		records := map[string]Record{}
		tools := map[string]ToolEvent{}
		for _, f := range files {
			name := f.Name()
			switch {
			case strings.HasPrefix(name, "r-") && strings.HasSuffix(name, ".meta"):
				requestID := strings.TrimSuffix(strings.TrimPrefix(name, "r-"), ".meta")
				if !ValidID(requestID) {
					return errors.New("sessions migration: invalid request identity")
				}
				var record Record
				if err := legacyOpenJSON(oldAEAD, filepath.Join(dir, name), meta.ID+"/"+requestID, &record); err != nil {
					return fmt.Errorf("sessions migration: record %s: %w", name, err)
				}
				if record.ID != requestID || record.SessionID != meta.ID {
					return errors.New("sessions migration: record identity mismatch")
				}
				records[requestID] = record
			case strings.HasPrefix(name, "t-") && strings.HasSuffix(name, ".meta"):
				var tool ToolEvent
				if err := legacyOpenJSON(oldAEAD, filepath.Join(dir, name), meta.ID+"/tool/"+name, &tool); err != nil {
					return fmt.Errorf("sessions migration: tool %s: %w", name, err)
				}
				if !validTool(tool) || name != toolFile(tool.ID) || tool.Source != "client_reported" || tool.ReportedAt.IsZero() {
					return errors.New("sessions migration: invalid tool event")
				}
				tools[tool.ID] = tool
			}
		}
		// Import bodies and re-encrypt everything.
		metaPlain, err := encodeMeta(meta)
		if err != nil {
			return err
		}
		metaSealed, err := db.Seal(metaPlain, "smeta:"+meta.ID)
		if err != nil {
			return err
		}
		tx, err := db.SQL().Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO sessions_meta(id, meta) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET meta = excluded.meta`, meta.ID, metaSealed); err != nil {
			tx.Rollback()
			return err
		}
		for requestID, record := range records {
			recordPlain, err := encodeRecord(record)
			if err != nil {
				tx.Rollback()
				return err
			}
			recordSealed, err := db.Seal(recordPlain, "srec:"+meta.ID+"/"+requestID)
			if err != nil {
				tx.Rollback()
				return err
			}
			if _, err := tx.Exec(`INSERT INTO session_records(session_id, request_id, record) VALUES(?, ?, ?) ON CONFLICT(session_id, request_id) DO UPDATE SET record = excluded.record`, meta.ID, requestID, recordSealed); err != nil {
				tx.Rollback()
				return err
			}
			for _, direction := range []string{"request", "response"} {
				bodyPath := filepath.Join(dir, "r-"+requestID+"."+direction)
				old, err := os.ReadFile(bodyPath)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					tx.Rollback()
					return err
				}
				body, err := RewriteBody(old, oldAEAD, newAEAD, meta.ID+"/"+requestID+"/"+direction)
				if err != nil {
					tx.Rollback()
					return fmt.Errorf("sessions migration: body %s: %w", bodyPath, err)
				}
				if _, err := tx.Exec(`INSERT INTO session_bodies(session_id, request_id, direction, body) VALUES(?, ?, ?, ?) ON CONFLICT(session_id, request_id, direction) DO UPDATE SET body = excluded.body`, meta.ID, requestID, direction, body); err != nil {
					tx.Rollback()
					return err
				}
			}
		}
		for toolID, tool := range tools {
			toolPlain, err := json.Marshal(tool)
			if err != nil {
				tx.Rollback()
				return err
			}
			toolSealed, err := db.Seal(string(toolPlain), "stool:"+meta.ID+"/"+toolID)
			if err != nil {
				tx.Rollback()
				return err
			}
			if _, err := tx.Exec(`INSERT INTO session_tools(session_id, tool_id, event) VALUES(?, ?, ?) ON CONFLICT(session_id, tool_id) DO UPDATE SET event = excluded.event`, meta.ID, toolID, toolSealed); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return db.MarkMigrated("sessions_v1")
}

// sessionDir recomputes the legacy session directory name (also used as the
// legacy session.meta encryption context).
func sessionDir(id string) string {
	hash := sha256.Sum256([]byte(id))
	return "session-" + hex.EncodeToString(hash[:])
}

// toolHash recomputes the legacy tool filename hash.
func toolHash(id string) string {
	hash := sha256.Sum256([]byte(id))
	return hex.EncodeToString(hash[:])
}

// toolFile recomputes the legacy tool filename used as an encryption context.
func toolFile(id string) string {
	return "t-" + toolHash(id) + ".meta"
}

// legacyOpenJSON reads and authenticates one legacy encrypted JSON file.
func legacyOpenJSON(aead cipher.AEAD, path, context string, value any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) < aead.NonceSize()+aead.Overhead() {
		return errors.New("invalid encrypted session metadata")
	}
	plain, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], []byte(context))
	if err != nil {
		return errors.New("session metadata authentication failed")
	}
	return json.Unmarshal(plain, value)
}

// legacySessionsAEAD rebuilds the old directory-store cipher (key from env or
// <dir>.key) so pre-database archives can be decrypted once during migration.
func legacySessionsAEAD(dir, keyEnv string) (cipher.AEAD, error) {
	var key []byte
	var err error
	if keyEnv != "" {
		if value := os.Getenv(keyEnv); value != "" {
			key, err = base64.StdEncoding.DecodeString(value)
			if err != nil || len(key) != 32 {
				return nil, errors.New("legacy session key must be 32 bytes encoded as standard base64")
			}
		}
	}
	if len(key) != 32 {
		key, err = os.ReadFile(dir + ".key")
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("legacy session key missing or invalid (%s)", filepath.Base(dir)+".key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
