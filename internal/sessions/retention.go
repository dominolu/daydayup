package sessions

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Retention configures pruning of archived session bodies. Bodies are the
// bulk of the archive (every request/response body is stored); the retention
// policy deletes old overflow bodies while keeping records, titles and
// session metadata intact. Zero values disable the corresponding pruner.
type Retention struct {
	// Days deletes bodies of requests older than this many days (0 = off).
	Days int `json:"days,omitempty"`
	// MaxBodyMB keeps only the newest bodies within a total of this many
	// megabytes of stored body content (0 = off).
	MaxBodyMB int `json:"max_body_mb,omitempty"`
	// BodyFiles enables on-disk body files with a database index only
	// (nil = enabled). False keeps body BLOBs inside the database.
	BodyFiles *bool `json:"body_files,omitempty"`
}

// ShouldPrune reports whether any policy is active.
func (r Retention) ShouldPrune() bool { return r.Days > 0 || r.MaxBodyMB > 0 }

// BodyFilesEnabled reports the effective storage mode (default: files).
func (r Retention) BodyFilesEnabled() bool { return r.BodyFiles == nil || *r.BodyFiles }

type bodyOwner struct {
	session string
	request string
	bytes   int64
	started time.Time
}

// Prune applies the configured retention policy and reports how many body
// rows were deleted and the estimated freed body bytes. It never touches the
// records: session metadata, titles and request/response metadata survive.
func (s *Store) Prune() (purged int64, freedBytes int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retentionDays > 0 {
		n, b, e := s.pruneByAgeLocked()
		if e != nil {
			return purged, freedBytes, e
		}
		purged += n
		freedBytes += b
	}
	if s.maxBodyMB > 0 {
		n, b, e := s.pruneByCapacityLocked()
		if e != nil {
			return purged, freedBytes, e
		}
		purged += n
		freedBytes += b
	}
	return purged, freedBytes, nil
}

// pruneByAgeLocked deletes bodies of requests whose last activity predates
// the retention window. Both pruners read the in-memory entries (already
// decrypted at load) so no scan of the multi-gigabyte body table is needed.
func (s *Store) pruneByAgeLocked() (int64, int64, error) {
	cutoff := time.Now().AddDate(0, 0, -s.retentionDays).UTC()
	var stale []bodyOwner
	for _, e := range s.entries {
		for id, rec := range e.records {
			if s.pruned[[2]string{e.meta.ID, id}] {
				continue
			}
			// A request still being recorded after a restart must never be
			// pruned even when its StartedAt is old.
			if rec.Outcome == "running" {
				continue
			}
			t := rec.FinishedAt
			if t.IsZero() {
				t = rec.StartedAt
			}
			if t.IsZero() || t.After(cutoff) {
				continue
			}
			stale = append(stale, bodyOwner{session: e.meta.ID, request: id, bytes: rec.Request.Bytes + rec.Response.Bytes})
		}
	}
	return s.deleteBodiesLocked(stale)
}

// pruneByCapacityLocked keeps the newest bodies within maxBodyMB of stored
// body content, deleting the oldest overflow.
func (s *Store) pruneByCapacityLocked() (int64, int64, error) {
	limit := int64(s.maxBodyMB) << 20
	if limit <= 0 {
		return 0, 0, nil
	}
	var owned []bodyOwner
	for _, e := range s.entries {
		for id, rec := range e.records {
			if s.pruned[[2]string{e.meta.ID, id}] {
				continue
			}
			bytes := rec.Request.Bytes + rec.Response.Bytes
			if bytes <= 0 {
				continue
			}
			owned = append(owned, bodyOwner{session: e.meta.ID, request: id, bytes: bytes, started: rec.StartedAt})
		}
	}
	// Newest first; ties break by session id for determinism.
	sort.Slice(owned, func(i, j int) bool {
		if !owned[i].started.Equal(owned[j].started) {
			return owned[i].started.After(owned[j].started)
		}
		return owned[i].session < owned[j].session
	})
	var total int64
	var overflow []bodyOwner
	for _, o := range owned {
		if total >= limit {
			overflow = append(overflow, o)
			continue
		}
		total += o.bytes
	}
	return s.deleteBodiesLocked(overflow)
}

// deleteBodiesLocked removes the body rows for the given request owners in
// bounded chunks (each chunk is one transaction), so a large first cut never
// holds the shared database connection for tens of seconds. Already-pruned
// pairs are remembered so later passes do not double-count deleted bodies.
func (s *Store) deleteBodiesLocked(owners []bodyOwner) (int64, int64, error) {
	var purged, freed int64
	for start := 0; start < len(owners); start += pruneChunk {
		end := start + pruneChunk
		if end > len(owners) {
			end = len(owners)
		}
		n, b, err := s.deleteChunk(owners[start:end])
		if err != nil {
			return purged, freed, err
		}
		purged += n
		freed += b
	}
	return purged, freed, nil
}

// pruneChunk bounds one retention delete transaction (rows are multi-MB
// blobs; deleting them rewrites many B-tree pages, so the chunk keeps each
// transaction short enough that request-path writes are not starved for long).
const pruneChunk = 250

func (s *Store) deleteChunk(owners []bodyOwner) (int64, int64, error) {
	if len(owners) == 0 {
		return 0, 0, nil
	}
	// File-backed bodies are reclaimed by deleting the files first (delete
	// semantics, no compaction needed); a crash in between leaves a dead
	// index row that reconcileFiles() removes. Database BLOB bodies are
	// removed with the index rows below. freedBytes counts only owners whose
	// archive files were actually present and removed.
	fileErrs := 0
	var freed int64
	for _, o := range owners {
		removed := false
		for _, dir := range []string{"request", "response"} {
			p := s.bodyPath(o.session, o.request, dir)
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if err := os.Remove(p); err != nil {
				fileErrs++
				continue
			}
			removed = true
		}
		if removed {
			freed += o.bytes
		}
	}
	tx, err := s.db.SQL().Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("retention: begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DROP TABLE IF EXISTS _retention_pairs`); err != nil {
		return 0, 0, fmt.Errorf("retention: temp: %w", err)
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE _retention_pairs(sid TEXT NOT NULL, rid TEXT NOT NULL)`); err != nil {
		return 0, 0, fmt.Errorf("retention: temp: %w", err)
	}
	// Multi-row inserts keep the statement count low.
	const batch = 400
	for start := 0; start < len(owners); start += batch {
		end := start + batch
		if end > len(owners) {
			end = len(owners)
		}
		var sb strings.Builder
		sb.WriteString(`INSERT INTO _retention_pairs VALUES `)
		args := make([]any, 0, (end-start)*2)
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteString(",")
			}
			sb.WriteString(`(?,?)`)
			args = append(args, owners[i].session, owners[i].request)
		}
		if _, err := tx.Exec(sb.String(), args...); err != nil {
			return 0, 0, fmt.Errorf("retention: temp insert: %w", err)
		}
	}
	res, err := tx.Exec(`DELETE FROM session_bodies WHERE (session_id, request_id) IN (SELECT sid, rid FROM _retention_pairs)`)
	if err != nil {
		return 0, 0, fmt.Errorf("retention: delete: %w", err)
	}
	purged, err := res.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("retention: rows: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE _retention_pairs`); err != nil {
		return 0, 0, fmt.Errorf("retention: temp drop: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("retention: commit: %w", err)
	}
	if fileErrs > 0 {
		return purged, 0, fmt.Errorf("retention: %d body files could not be removed", fileErrs)
	}
	for _, o := range owners {
		s.pruned[[2]string{o.session, o.request}] = true
	}
	return purged, freed, nil
}
