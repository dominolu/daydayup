package sessions

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	return s, dbPath
}
func record(t *testing.T, s *Store, id string, response []byte) {
	t.Helper()
	c, err := s.Begin(Record{ID: id, SessionID: "session-1", Provider: "sensenova", Model: "m", Request: Body{Complete: true}}, []byte("private request"), "")
	if err != nil {
		t.Fatal(err)
	}
	c.WriteResponse(response)
	c.AddAttempt(Attempt{AccountID: "a", Usage: Usage{Input: 4, Output: 2, Total: 6, Cached: 1, Known: true}})
	if err = c.Finish(Result{Status: 200, Outcome: "success", ResponseID: "resp-" + id, Response: Body{Complete: true}}); err != nil {
		t.Fatal(err)
	}
}
func TestEncryptedBodiesRestartAggregationAndIsolation(t *testing.T) {
	s, dbPath := testStore(t)
	body := bytes.Repeat([]byte("private output "), 14000)
	record(t, s, "r1", body)
	record(t, s, "r2", []byte("second output"))
	if err := s.BindAccount("session-1", "m", "a"); err != nil {
		t.Fatal(err)
	}
	// The database and its temp bodies must never hold plaintext.
	db, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(db, []byte("private")) {
		t.Fatal("plaintext in database")
	}
	info, _ := os.Stat(dbPath)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("database permissions: %o", info.Mode().Perm())
	}
	reopened, err := Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	sum, rows, total, err := reopened.Detail("session-1", 0, 1)
	if err != nil || total != 2 || len(rows) != 1 || sum.Usage.Total != 12 || sum.Usage.Cached != 2 || sum.PreferredAccounts["m"] != "a" {
		t.Fatalf("bad summary %+v %v", sum, err)
	}
	f, err := reopened.ReadBody("session-1", "r1", "response")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	f.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body mismatch: %v", err)
	}
	if _, err = reopened.ReadBody("other-session", "r1", "response"); !os.IsNotExist(err) {
		t.Fatal("cross-session body readable")
	}
	sid, aid, ok := reopened.ResolveResponse("sensenova", "resp-r1")
	if !ok || sid != "session-1" || aid != "a" {
		t.Fatal("response ownership lost")
	}
	if _, _, ok = reopened.ResolveResponse("other", "resp-r1"); ok {
		t.Fatal("cross-provider response resolved")
	}
	name := "新的会话名称"
	archive := true
	if err = reopened.Update(sid, &name, &archive); err != nil {
		t.Fatal(err)
	}
	list, total := reopened.List("新的", true, 0, 30)
	if total != 1 || list[0].Name != name {
		t.Fatal("archive/name filter")
	}
	if _, total = reopened.List("", false, 0, 30); total != 0 {
		t.Fatal("archived shown as active")
	}
	// A body row copied from another request must fail its authenticated
	// chunk context (every chunk is bound to its session/request/direction).
	var blob []byte
	if err := s.db.SQL().QueryRow(`SELECT body FROM session_bodies WHERE session_id = ? AND request_id = 'r1' AND direction = 'response'`, "session-1").Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.SQL().Exec(`UPDATE session_bodies SET body = ? WHERE session_id = ? AND request_id = 'r2' AND direction = 'response'`, blob, "session-1"); err != nil {
		t.Fatal(err)
	}
	f, err = s.ReadBody("session-1", "r2", "response")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(f)
	f.Close()
	if err == nil {
		t.Fatal("body substitution not detected")
	}
}
func TestInterruptedRequestAndTruncationAreVisible(t *testing.T) {
	s, dbPath := testStore(t)
	c, err := s.Begin(Record{ID: "unfinished", SessionID: "session-1"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	c.WriteResponse([]byte("partial"))
	// Simulate a crash: the capture is abandoned without Finish, so the
	// durable record stays "running" and the body sits in the temp dir.
	restarted, err := Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	summary, rows, _, err := restarted.Detail("session-1", 0, 30)
	if err != nil || rows[0].Outcome != "interrupted" || rows[0].Response.Complete || summary.UnknownUsageRequests != 1 {
		t.Fatalf("lost interrupted status: %+v %v", rows, err)
	}
	f, err := restarted.ReadBody("session-1", "unfinished", "response")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	f.Close()
	if string(got) != "partial" || err == nil {
		t.Fatal("truncation hidden")
	}
}
func TestConcurrentRecordsAndUnknownUsage(t *testing.T) {
	s, _ := testStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := base64.RawURLEncoding.EncodeToString([]byte{byte(i + 40)})
			record(t, s, id, []byte("result"))
		}()
	}
	wg.Wait()
	c, err := s.Begin(Record{ID: "unknown", SessionID: "session-1"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	c.AddAttempt(Attempt{AccountID: "a"})
	c.AddAttempt(Attempt{AccountID: "b", Usage: Usage{Total: 9, Known: true}})
	if err = c.Finish(Result{Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	sum, _, _, _ := s.Detail("session-1", 0, 30)
	if sum.Requests != 13 || sum.Usage.Total != 81 || sum.UnknownAttempts != 1 || sum.UnknownUsageRequests != 1 {
		t.Fatalf("bad accounting %+v", sum)
	}
}
func TestLostOrWrongKeyDoesNotCreateNewKeyOverData(t *testing.T) {
	s, dbPath := testStore(t)
	record(t, s, "r1", nil)
	key, err := os.ReadFile(dbPath + ".key")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_DB_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	if _, err = Open(dbPath, ""); err == nil {
		t.Fatal("wrong key accepted")
	}
	if err = os.Remove(dbPath + ".key"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_DB_KEY", "")
	if _, err = Open(dbPath, ""); err == nil {
		t.Fatal("lost key replaced")
	}
	t.Setenv("GATEWAY_DB_KEY", base64.StdEncoding.EncodeToString(key))
	reopened, err := Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, total, _ := reopened.Detail("session-1", 0, 30); total != 1 {
		t.Fatal("data lost after key rotation")
	}
}
