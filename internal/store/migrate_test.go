package store

import (
	"bytes"
	"path/filepath"
	"testing"
)

// TestMigrateFromEmptyState is a smoke test for the store package itself:
// opening a fresh database, writing and re-reading through the generic
// seal/open helpers, and restarting on the same file with the same key.
func TestRoundTripAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := db.Seal(`{"secret":"s3cret"}`, "ctx:1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("s3cret")) {
		t.Fatal("plaintext leaked into sealed value")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen: same key file, same row must decrypt.
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	plain, err := db2.Open(sealed, "ctx:1")
	if err != nil || plain != `{"secret":"s3cret"}` {
		t.Fatalf("round trip: %q %v", plain, err)
	}
	if _, err := db2.Open(sealed, "ctx:2"); err == nil {
		t.Fatal("context binding not enforced")
	}
}
