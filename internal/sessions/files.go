package sessions

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Body-file storage: archived request/response bodies live on disk, one
// directory per session and one chunked-encrypted file per request direction.
// The gateway database keeps only an index (session_bodies rows with an empty
// body column); the byte format of each file is identical to the legacy BLOB
// content, so readers and writers reuse the existing chunked encryption.
//
// Deleting a body is deleting its files (no compaction or vacuuming needed).
// Files are written by renaming the transient capture file into place, so the
// move is atomic; a crash between the rename and the index insert leaves an
// orphan file that ReconcileFiles() indexes on the next maintenance pass.

// bodySessionDir derives the per-session archive directory from the session
// id. Session ids are restricted to [A-Za-z0-9_.:-]{1,128}; the "s-" prefix
// (plus the ValidID guard below) makes the name reversible for maintenance
// scans while never colliding with "." or ".." — unlike a bare id, which a
// client-controlled value like ".." could turn into a path traversal.
func bodySessionDir(id string) string {
	if !ValidID(id) || id == "." || id == ".." {
		return ""
	}
	return "s-" + id
}

// bodyFilesRoot is the body archive root: <db dir>/session-files.
func bodyFilesRoot(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "session-files")
}

// bodyFilePath returns the archive file for one request direction under root.
func bodyFilePath(root, sessionID, requestID, direction string) string {
	return filepath.Join(root, bodySessionDir(sessionID), "r-"+requestID+"."+direction)
}

// bodyPath returns the archive file for one request direction.
func (s *Store) bodyPath(sessionID, requestID, direction string) string {
	return bodyFilePath(bodyFilesRoot(s.dbPath), sessionID, requestID, direction)
}

// filesRoot is the body archive root for this store.
func (s *Store) filesRoot() string { return bodyFilesRoot(s.dbPath) }

// writeBodyFile durably writes one chunked-encrypted body file (atomic rename).
func writeBodyFile(root, sessionID, requestID, direction string, sealed []byte) error {
	dst := bodyFilePath(root, sessionID, requestID, direction)
	if dir := filepath.Dir(dst); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ensureBodyIndex records the existence of one archived body file in the
// index table with an empty body column (empty column = file-backed body).
// The empty string (not an empty []byte) avoids the driver mapping empty
// bytes to NULL, which would violate the NOT NULL column.
func (s *Store) ensureBodyIndex(sessionID, requestID, direction string) error {
	_, err := s.db.SQL().Exec(`INSERT INTO session_bodies(session_id, request_id, direction, body) VALUES(?, ?, ?, ?) ON CONFLICT(session_id, request_id, direction) DO UPDATE SET body = excluded.body`, sessionID, requestID, direction, "")
	return err
}

// storedBodyBytes returns the raw chunked-encrypted body bytes for one
// request direction: the legacy BLOB column when it still holds content,
// otherwise the archive file (file-backed mode). os.ErrNotExist when the body
// is not stored at all.
func (s *Store) storedBodyBytes(sessionID, requestID, direction string) ([]byte, error) {
	var sealed []byte
	err := s.db.SQL().QueryRow(`SELECT body FROM session_bodies WHERE session_id = ? AND request_id = ? AND direction = ?`, sessionID, requestID, direction).Scan(&sealed)
	if err != nil {
		return nil, err
	}
	if len(sealed) > 0 {
		return sealed, nil
	}
	return os.ReadFile(s.bodyPath(sessionID, requestID, direction))
}

// BodyFiles reports whether this store archives bodies on disk.
func (s *Store) BodyFiles() bool { return s.bodyFiles }

// MigrateBodiesToFiles exports any remaining body BLOBs from the database
// into the per-session archive files. No-op once the marker is set.
func (s *Store) MigrateBodiesToFiles() error { return MigrateBodiesToFiles(s.db, s.dbPath) }

// ReconcileFiles aligns the archive directory with the index table:
//  1. orphan files (present on disk but not indexed, left by a crash between
//     rename and index insert) get index rows;
//  2. dead index rows (file-backed rows whose file no longer exists, left by
//     a crash between file delete and index delete) are removed.
//
// It is idempotent and safe to run concurrently with normal capture traffic.
func (s *Store) ReconcileFiles() (added, removed int, err error) {
	if !s.bodyFiles {
		return 0, 0, nil
	}
	err = filepath.WalkDir(s.filesRoot(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		sessionID, requestID, direction, ok := parseBodyFileName(path)
		if !ok {
			return nil
		}
		var sealed []byte
		qerr := s.db.SQL().QueryRow(`SELECT body FROM session_bodies WHERE session_id = ? AND request_id = ? AND direction = ?`, sessionID, requestID, direction).Scan(&sealed)
		if qerr != nil {
			if !errors.Is(qerr, sql.ErrNoRows) {
				return qerr
			}
			if err := s.ensureBodyIndex(sessionID, requestID, direction); err != nil {
				return err
			}
			added++
		}
		return nil
	})
	if err != nil {
		return added, removed, err
	}
	rows, err := s.db.SQL().Query(`SELECT session_id, request_id, direction FROM session_bodies WHERE length(body) = 0`)
	if err != nil {
		return added, removed, err
	}
	defer rows.Close()
	var dead [][3]string
	for rows.Next() {
		var sid, rid, dir string
		if err := rows.Scan(&sid, &rid, &dir); err != nil {
			return added, removed, err
		}
		if _, err := os.Stat(s.bodyPath(sid, rid, dir)); os.IsNotExist(err) {
			dead = append(dead, [3]string{sid, rid, dir})
		}
	}
	if err := rows.Err(); err != nil {
		return added, removed, err
	}
	for _, r := range dead {
		if _, err := s.db.SQL().Exec(`DELETE FROM session_bodies WHERE session_id = ? AND request_id = ? AND direction = ?`, r[0], r[1], r[2]); err != nil {
			return added, removed, err
		}
		removed++
	}
	return added, removed, nil
}

// parseBodyFileName decodes a session-files path of the form
// <root>/s-<session>/r-<request>.<direction> back into its parts.
func parseBodyFileName(path string) (sessionID, requestID, direction string, ok bool) {
	dir := filepath.Base(filepath.Dir(path))
	if !strings.HasPrefix(dir, "s-") {
		return "", "", "", false
	}
	sessionID = strings.TrimPrefix(dir, "s-")
	if !ValidID(sessionID) {
		return "", "", "", false
	}
	name := filepath.Base(path)
	if !strings.HasPrefix(name, "r-") {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(name, "r-")
	i := strings.LastIndex(rest, ".")
	if i <= 0 {
		return "", "", "", false
	}
	requestID, direction = rest[:i], rest[i+1:]
	if !ValidID(requestID) || (direction != "request" && direction != "response") {
		return "", "", "", false
	}
	return sessionID, requestID, direction, true
}
