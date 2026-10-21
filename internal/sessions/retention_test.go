package sessions

import (
	"path/filepath"
	"testing"
	"time"
)

func testOpen(t *testing.T, retention Retention) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	// Seed the database through a plain store first (records must exist in
	// the DB for the reopening store to load them into memory).
	seeder, err := Open(dbPath, "")
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	seed := func(metaID, requestID string, finished time.Time, reqBytes, respBytes int64) {
		t.Helper()
		if err := seeder.saveMeta(sessionMeta{ID: metaID, Name: metaID}); err != nil {
			t.Fatalf("saveMeta: %v", err)
		}
		rec := Record{
			ID:         requestID,
			SessionID:  metaID,
			StartedAt:  finished.Add(-time.Minute),
			FinishedAt: finished,
			Request:    Body{Bytes: reqBytes, Complete: true},
			Response:   Body{Bytes: respBytes, Complete: true},
		}
		if err := seeder.saveRecord(rec); err != nil {
			t.Fatalf("saveRecord: %v", err)
		}
		for _, dir := range []string{"request", "response"} {
			// File-backed storage: write an archive file and an empty-column
			// index row (content bytes are irrelevant to pruning).
			if err := writeBodyFile(bodyFilesRoot(dbPath), metaID, requestID, dir, []byte("x")); err != nil {
				t.Fatalf("write body file: %v", err)
			}
			if err := seeder.ensureBodyIndex(metaID, requestID, dir); err != nil {
				t.Fatalf("ensure index: %v", err)
			}
		}
	}
	seed("sess-old", "req-old", time.Now().AddDate(0, 0, -40), 300<<20, 100<<20)
	seed("sess-new", "req-new", time.Now(), 1<<20, 1<<20)
	seed("sess-mid", "req-mid", time.Now().AddDate(0, 0, -10), 200<<20, 200<<20)

	store, err := OpenWithRetention(dbPath, Retention{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return store, dbPath
}

func bodyRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.SQL().QueryRow(`SELECT count(*) FROM session_bodies`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestPruneByAge(t *testing.T) {
	s, _ := testOpen(t, Retention{})
	s.retentionDays = 30
	if n := bodyRows(t, s); n != 6 {
		t.Fatalf("setup rows = %d, want 6", n)
	}
	purged, freed, err := s.Prune()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if purged != 2 || freed != 400<<20 {
		t.Fatalf("purged = %d rows, freed = %d bytes; want 2 rows / 400MiB", purged, freed)
	}
	if n := bodyRows(t, s); n != 4 {
		t.Fatalf("rows after age prune = %d, want 4 (oldest session bodies removed)", n)
	}
}

func TestPruneByCapacity(t *testing.T) {
	// Keep-newest log policy: items are added until the cap is reached; the
	// older tail overflows. sess-new (2MiB) + sess-mid (400MiB) fit under the
	// 256MiB running cap once accumulated (2MiB < cap), so only sess-old drops.
	s, _ := testOpen(t, Retention{})
	s.maxBodyMB = 256
	purged, freed, err := s.Prune()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if purged != 2 {
		t.Fatalf("purged = %d rows, want 2 (sess-old bodies)", purged)
	}
	if freed != 400<<20 {
		t.Fatalf("freed = %d bytes, want 400MiB", freed)
	}
	if n := bodyRows(t, s); n != 4 {
		t.Fatalf("rows after capacity prune = %d, want 4", n)
	}
}

func TestPruneDisabled(t *testing.T) {
	s, _ := testOpen(t, Retention{})
	purged, freed, err := s.Prune()
	if err != nil || purged != 0 || freed != 0 {
		t.Fatalf("disabled prune: purged=%d freed=%d err=%v", purged, freed, err)
	}
	if n := bodyRows(t, s); n != 6 {
		t.Fatalf("rows = %d, want 6", n)
	}
}

func TestPruneIdempotent(t *testing.T) {
	s, _ := testOpen(t, Retention{})
	s.retentionDays, s.maxBodyMB = 30, 256
	if _, _, err := s.Prune(); err != nil {
		t.Fatalf("first prune: %v", err)
	}
	// Second pass must not double-delete or double-report.
	purged, freed, err := s.Prune()
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if purged != 0 || freed != 0 {
		t.Fatalf("second prune purged=%d freed=%d, want 0/0", purged, freed)
	}
	if n := bodyRows(t, s); n != 4 {
		t.Fatalf("rows after both prunes = %d, want 4", n)
	}
}
