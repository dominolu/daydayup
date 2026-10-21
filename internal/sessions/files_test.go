package sessions

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBodyFileRoundTrip writes bodies through the capture path and reads them
// back via ReadBody, verifying the per-session archive files hold the content
// and the database holds only an empty-column index.
func TestBodyFileRoundTrip(t *testing.T) {
	s, dbPath := testStore(t)
	if !s.BodyFiles() {
		t.Fatal("default storage mode must be file-backed")
	}
	body := []byte("private output stream")
	c, err := s.Begin(Record{ID: "r1", SessionID: "sess-f1", Provider: "sensenova", Model: "m", Request: Body{Complete: true}}, []byte("private request"), "")
	if err != nil {
		t.Fatal(err)
	}
	c.WriteResponse(body)
	if err := c.Finish(Result{Status: 200, Outcome: "success", Response: Body{Complete: true}}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"request", "response"} {
		p := filepath.Join(bodyFilesRoot(dbPath), bodySessionDir("sess-f1"), "r-r1."+dir)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing archive file %s: %v", p, err)
		}
		var sealed []byte
		if err := s.db.SQL().QueryRow(`SELECT body FROM session_bodies WHERE session_id = ? AND request_id = ? AND direction = ?`, "sess-f1", "r1", dir).Scan(&sealed); err != nil {
			t.Fatalf("index row missing: %v", err)
		}
		if len(sealed) != 0 {
			t.Fatalf("index row must have empty body column, got %d bytes", len(sealed))
		}
	}
	r, err := s.ReadBody("sess-f1", "r1", "request")
	if err != nil {
		t.Fatalf("ReadBody request: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != "private request" {
		t.Fatalf("request body mismatch: %q err=%v", got, err)
	}
	r, err = s.ReadBody("sess-f1", "r1", "response")
	if err != nil {
		t.Fatalf("ReadBody response: %v", err)
	}
	got, err = io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != string(body) {
		t.Fatalf("response body mismatch: %q err=%v", got, err)
	}
}

// TestPruneDeletesBodyFiles verifies retention deletes the archive files
// (delete-to-reclaim semantics) and the index rows together.
func TestPruneDeletesBodyFiles(t *testing.T) {
	s, dbPath := testStore(t)
	s.retentionDays = 30
	old := time.Now().AddDate(0, 0, -40)
	if err := s.saveMeta(sessionMeta{ID: "sess-old", Name: "sess-old"}); err != nil {
		t.Fatal(err)
	}
	rec := Record{ID: "req-old", SessionID: "sess-old", StartedAt: old, FinishedAt: old, Request: Body{Bytes: 10, Complete: true}, Response: Body{Bytes: 10, Complete: true}}
	if err := s.saveRecord(rec); err != nil {
		t.Fatal(err)
	}
	s.entries["sess-old"] = &entry{meta: sessionMeta{ID: "sess-old"}, records: map[string]Record{"req-old": rec}, tools: map[string]ToolEvent{}}
	for _, dir := range []string{"request", "response"} {
		tmp := filepath.Join(t.TempDir(), "x")
		w, err := createBody(s.db.AEAD(), tmp, "sess-old/req-old/"+dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := (&Capture{store: s}).importBody("sess-old", "req-old", dir, tmp); err != nil {
			t.Fatal(err)
		}
	}
	purged, _, err := s.Prune()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if purged != 2 {
		t.Fatalf("purged = %d rows, want 2", purged)
	}
	for _, dir := range []string{"request", "response"} {
		p := filepath.Join(bodyFilesRoot(dbPath), bodySessionDir("sess-old"), "r-req-old."+dir)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("archive file still present after prune: %v", err)
		}
	}
	var n int
	if err := s.db.SQL().QueryRow(`SELECT count(*) FROM session_bodies`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("index rows after prune = %d, want 0", n)
	}
}

// TestMigrateBodiesToFiles exports legacy database BLOBs into archive files,
// leaving empty-column index rows, and the bodies stay readable afterwards.
func TestMigrateBodiesToFiles(t *testing.T) {
	s, dbPath := testStore(t)
	// Write a legacy BLOB row directly (pre-file storage state).
	tmp := filepath.Join(t.TempDir(), "x")
	w, err := createBody(s.db.AEAD(), tmp, "sess-1/r1/request")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("legacy content")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.SQL().Exec(`INSERT INTO session_bodies(session_id, request_id, direction, body) VALUES(?, ?, ?, ?)`, "sess-1", "r1", "request", sealed); err != nil {
		t.Fatal(err)
	}
	rec := Record{ID: "r1", SessionID: "sess-1", StartedAt: time.Now(), FinishedAt: time.Now(), Request: Body{Bytes: 14, Complete: true}, Response: Body{Complete: true}}
	if err := s.saveMeta(sessionMeta{ID: "sess-1", Name: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveRecord(rec); err != nil {
		t.Fatal(err)
	}
	s.entries["sess-1"] = &entry{meta: sessionMeta{ID: "sess-1"}, records: map[string]Record{"r1": rec}, tools: map[string]ToolEvent{}}
	if err := s.MigrateBodiesToFiles(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p := filepath.Join(bodyFilesRoot(dbPath), bodySessionDir("sess-1"), "r-r1.request")
	fileSealed, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("archive file missing: %v", err)
	}
	if string(fileSealed) != string(sealed) {
		t.Fatal("archive file content differs from exported blob")
	}
	var col []byte
	if err := s.db.SQL().QueryRow(`SELECT body FROM session_bodies WHERE session_id = ? AND request_id = ? AND direction = ?`, "sess-1", "r1", "request").Scan(&col); err != nil {
		t.Fatal(err)
	}
	if len(col) != 0 {
		t.Fatalf("column not emptied, %d bytes remain", len(col))
	}
	if done, _ := s.db.Migrated("bodies_files_v1"); !done {
		t.Fatal("migration marker not set")
	}
	r, err := s.ReadBody("sess-1", "r1", "request")
	if err != nil {
		t.Fatalf("ReadBody after migrate: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != "legacy content" {
		t.Fatalf("body after migrate: %q err=%v", got, err)
	}
	// Idempotent: second run is a no-op.
	if err := s.MigrateBodiesToFiles(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// TestReconcileFiles indexes orphan files left by a crash between rename and
// index insert, and drops dead index rows whose file is already gone.
func TestReconcileFiles(t *testing.T) {
	s, dbPath := testStore(t)
	root := bodyFilesRoot(dbPath)
	// Orphan file with no index row.
	orphan := filepath.Join(root, bodySessionDir("sess-orphan"), "r-ro.request")
	if err := os.MkdirAll(filepath.Dir(orphan), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	// Dead index row with no file.
	if err := s.ensureBodyIndex("sess-dead", "rd", "response"); err != nil {
		t.Fatal(err)
	}
	added, removed, err := s.ReconcileFiles()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if added != 1 || removed != 1 {
		t.Fatalf("added=%d removed=%d, want 1/1", added, removed)
	}
	var sealed []byte
	if err := s.db.SQL().QueryRow(`SELECT body FROM session_bodies WHERE session_id = ? AND request_id = ? AND direction = ?`, "sess-orphan", "ro", "request").Scan(&sealed); err != nil {
		t.Fatalf("orphan not indexed: %v", err)
	}
	if len(sealed) != 0 {
		t.Fatal("orphan index row must have empty body column")
	}
	var n int
	if err := s.db.SQL().QueryRow(`SELECT count(*) FROM session_bodies WHERE session_id = ? AND request_id = ?`, "sess-dead", "rd").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("dead index row not removed")
	}
}

// TestBodyFilesDisabled keeps the legacy database-BLOB storage mode when
// body_files is false.
func TestBodyFilesDisabled(t *testing.T) {
	f := false
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	s, err := OpenWithRetention(dbPath, Retention{BodyFiles: &f})
	if err != nil {
		t.Fatal(err)
	}
	if s.BodyFiles() {
		t.Fatal("body_files=false must keep database storage")
	}
	c, err := s.Begin(Record{ID: "r1", SessionID: "sess-1", Provider: "sensenova", Model: "m", Request: Body{Complete: true}}, []byte("private request"), "")
	if err != nil {
		t.Fatal(err)
	}
	c.WriteResponse([]byte("output"))
	if err := c.Finish(Result{Status: 200, Outcome: "success", Response: Body{Complete: true}}); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err := s.db.SQL().QueryRow(`SELECT body FROM session_bodies WHERE session_id = ? AND request_id = ? AND direction = ?`, "sess-1", "r1", "request").Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if len(sealed) == 0 {
		t.Fatal("database mode must store body content in the column")
	}
	if _, err := os.Stat(filepath.Join(bodyFilesRoot(dbPath), bodySessionDir("sess-1"))); !os.IsNotExist(err) {
		t.Fatal("database mode must not create archive files")
	}
}
