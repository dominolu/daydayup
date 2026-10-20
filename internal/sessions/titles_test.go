package sessions

import (
	"bytes"
	"os"
	"sync"
	"testing"
	"time"
)

func titleCapture(t *testing.T, s *Store, id string, started time.Time) *Capture {
	t.Helper()
	c, err := s.Begin(Record{ID: id, SessionID: "chat", Provider: "sensenova", StartedAt: started, Request: Body{Complete: true}, Title: &Title{Detector: "test", Association: "session_id", Status: "pending"}}, []byte("private request"), "")
	if err != nil {
		t.Fatal(err)
	}
	c.WriteResponse([]byte("private response"))
	c.AddAttempt(Attempt{AccountID: "a", Usage: Usage{Input: 2, Output: 1, Total: 3, Known: true}})
	return c
}

func TestTitleSeedHasOnlyOneConcurrentClaimant(t *testing.T) {
	s, _ := testStore(t)
	hashes := MessageHashes([]string{"same prompt"})
	c, err := s.Begin(Record{ID: "title", SessionID: "seed", Provider: "sensenova", Title: &Title{Detector: "test", Association: "unmatched", Status: "pending", SourceMessages: hashes}}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	finishTitle(t, c, "pending name")
	var wg sync.WaitGroup
	claims := make(chan string, 2)
	for _, id := range []string{"main-one", "main-two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sid, _ := s.ResolveTitleSeed("sensenova", "Unknown", hashes, id)
			if sid != "" {
				claims <- id
			}
		}(id)
	}
	wg.Wait()
	close(claims)
	count := 0
	for range claims {
		count++
	}
	if count != 1 {
		t.Fatalf("pending title adopted %d times", count)
	}
}
func finishTitle(t *testing.T, c *Capture, text string) {
	t.Helper()
	if err := c.Finish(Result{Status: 200, Outcome: "success", GeneratedTitle: text, Response: Body{Complete: true}}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentTitleOrderAndManualRename(t *testing.T) {
	s, dbPath := testStore(t)
	base := time.Now().Add(-time.Minute)
	old := titleCapture(t, s, "old", base)
	newer := titleCapture(t, s, "new", base.Add(time.Second))
	finishTitle(t, newer, "新的标题")
	finishTitle(t, old, "迟到的旧标题")
	sum, _, _, _ := s.Detail("chat", 0, 30)
	oldRecord, _ := s.Record("chat", "old")
	if sum.Name != "新的标题" || oldRecord.Title.Status != "superseded" || sum.TitleRequests != 2 || sum.Usage.Total != 6 {
		t.Fatalf("old result overwrote new %+v %+v", sum, oldRecord.Title)
	}
	// Simulate a crash between metadata commit and the final title audit update.
	newRecord, _ := s.Record("chat", "new")
	newRecord.Title.Status = "pending"
	if err := s.saveRecord(newRecord); err != nil {
		t.Fatal(err)
	}
	var err error
	s, err = Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	newRecord, _ = s.Record("chat", "new")
	sum, _, _, _ = s.Detail("chat", 0, 30)
	if sum.Name != "新的标题" || newRecord.Title.Status != "applied" {
		t.Fatal("pending rename did not recover")
	}
	pending := titleCapture(t, s, "pending", base.Add(2*time.Second))
	manual := "人工保留的名称"
	if err = s.Update("chat", &manual, nil); err != nil {
		t.Fatal(err)
	}
	finishTitle(t, pending, "自动标题不覆盖人工名称")
	sum, _, _, _ = s.Detail("chat", 0, 30)
	r, _ := s.Record("chat", "pending")
	if sum.Name != manual || sum.NameInfo.Source != "manual" || r.Title.Status != "manual_name_preserved" {
		t.Fatal("manual rename lost")
	}
	db, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(db, []byte("新的标题")) || bytes.Contains(db, []byte(manual)) {
		t.Fatal("plaintext title in database")
	}
}

func TestTitleIncompleteResponseCannotRename(t *testing.T) {
	s, dbPath := testStore(t)
	c := titleCapture(t, s, "incomplete", time.Now())
	if err := c.Finish(Result{Status: 200, Outcome: "success", GeneratedTitle: "完整性未知的标题", Response: Body{Complete: false}}); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	sum, rows, _, _ := s.Detail("chat", 0, 30)
	if sum.Name != "chat" || rows[0].Title.Status != "failed" {
		t.Fatal("incomplete title was used")
	}
}

func TestLegacyPromptIndexAndNameProtection(t *testing.T) {
	s, dbPath := testStore(t)
	c, err := s.Begin(Record{ID: "legacy", SessionID: "legacy-chat", Provider: "sensenova", Path: "/v1/chat/completions", Request: Body{Complete: true}}, []byte(`{"messages":[{"role":"user","content":"精确原始消息"}]}`), "已有名称")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Finish(Result{Status: 200, Outcome: "success", Response: Body{Complete: true}}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Record("legacy-chat", "legacy")
	r.UserMessages = nil
	r.Purpose = ""
	if err = s.saveRecord(r); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	sid, _, note := s.ResolveTitleSession("sensenova", "DSH", MessageHashes([]string{"精确原始消息"}))
	if sid != "legacy-chat" || note != "user_messages" {
		t.Fatal("legacy fingerprint missing")
	}
	sum, _, _, _ := s.Detail(sid, 0, 30)
	if sum.NameInfo.Source != "manual" {
		t.Fatal("legacy name not protected")
	}
	if sid, _, _ := s.ResolveTitleSession("different", "DSH", MessageHashes([]string{"精确原始消息"})); sid != "" {
		t.Fatal("cross-provider title association")
	}
}

func TestTitleNormalizationAndMultimodalIsolation(t *testing.T) {
	for _, raw := range []string{"", `{"other":"no title"}`, "two\nlines", "two\u2028lines", "two\u2029lines", "title\u200b", string([]byte{0xff}), "```json\n{}\n```"} {
		if _, ok := NormalizeTitle(raw); ok {
			t.Fatalf("invalid title accepted %q", raw)
		}
	}
	if got, ok := NormalizeTitle(`{"title":" 修复接口 "}`); !ok || got != "修复接口" {
		t.Fatal("JSON title not decoded")
	}
	if hashes := MessageHashes(HumanMessages("/v1/chat/completions", []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"same text"},{"type":"image_url","image_url":{"url":"image"}}]}]}`))); len(hashes) != 0 {
		t.Fatal("image context reduced to text identity")
	}
}
