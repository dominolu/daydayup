package sessions

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daydayup/internal/store"
)

// TestMigrateLegacySessionDir builds a pre-database session archive (encrypted
// meta/records/bodies with the old key) and verifies MigrateDB moves it into
// the database without data loss.
func TestMigrateLegacySessionDir(t *testing.T) {
	dir := t.TempDir()
	legacyDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	if err := os.WriteFile(legacyDir+".key", key, 0600); err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	legacy, _ := cipher.NewGCM(block)

	sessionID := "legacy-chat"
	dirName := "session-" + func() string { h := sha256.Sum256([]byte(sessionID)); return hex.EncodeToString(h[:]) }()
	legacySession := filepath.Join(legacyDir, dirName)
	if err := os.MkdirAll(legacySession, 0700); err != nil {
		t.Fatal(err)
	}
	meta := sessionMeta{ID: sessionID, Name: "旧会话", CreatedAt: time.Now(), PreferredAccounts: map[string]string{}}
	sealLegacy(t, legacy, filepath.Join(legacySession, "session.meta"), dirName, meta)
	body := []byte(`{"messages":[{"role":"user","content":"旧消息原文"}]}`)
	record := Record{ID: "req1", SessionID: sessionID, Provider: "sensenova", Model: "m", Path: "/v1/chat/completions", StartedAt: time.Now(), FinishedAt: time.Now(), Status: 200, Outcome: "success", Request: Body{ContentType: "application/json", Bytes: int64(len(body)), Complete: true}, Response: Body{Complete: true}, Usage: Usage{Input: 4, Output: 2, Total: 6, Known: true}}
	sealLegacy(t, legacy, filepath.Join(legacySession, "r-req1.meta"), sessionID+"/req1", record)
	w, err := createBody(legacy, filepath.Join(legacySession, "r-req1.request"), sessionID+"/req1/request")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	tool := ToolEvent{ID: "call-1", RequestID: "req1", Name: "旧工具", DurationMS: 5, Status: "success", ReportedAt: time.Now(), Source: "client_reported"}
	toolName := toolFile(tool.ID)
	sealLegacy(t, legacy, filepath.Join(legacySession, toolName), sessionID+"/tool/"+toolName, tool)

	dbPath := filepath.Join(dir, "gateway.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := MigrateDB(db, legacyDir, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := MigrateDB(db, legacyDir, ""); err != nil {
		t.Fatalf("migration not idempotent: %v", err)
	}
	s, err := Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	sum, rows, total, err := s.Detail(sessionID, 0, 30)
	if err != nil || total != 1 || sum.Name != "旧会话" || rows[0].Outcome != "success" {
		t.Fatalf("migrated session wrong: %+v %v", sum, err)
	}
	// The body (chunk-encrypted with the legacy key) must be readable after
	// re-encryption, and the fingerprint must be rebuilt from it.
	got, err := s.readBodyBytes(sessionID, "req1", "request")
	if err != nil || string(got) != string(body) {
		t.Fatalf("migrated body wrong: %q %v", got, err)
	}
	if sid, _, note := s.ResolveTitleSession("sensenova", "DSH", MessageHashes([]string{"旧消息原文"})); sid != sessionID || note != "user_messages" {
		t.Fatalf("legacy fingerprint not rebuilt: %q %q", sid, note)
	}
	tools, total, err := s.Tools(sessionID, "req1", 0, 30)
	if err != nil || total != 1 || tools[0].ID != tool.ID {
		t.Fatalf("migrated tool wrong: %+v %v", tools, err)
	}
}

// sealLegacy encrypts a JSON value into the old per-file format.
func sealLegacy(t *testing.T, aead cipher.AEAD, path, context string, value any) {
	t.Helper()
	plain, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	sealed := aead.Seal(nonce, nonce, plain, []byte(context))
	if err := os.WriteFile(path, sealed, 0600); err != nil {
		t.Fatal(err)
	}
}
