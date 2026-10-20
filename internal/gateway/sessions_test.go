package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"daydayup/internal/sessions"
)

func enableSessions(t *testing.T, s *Server) {
	t.Helper()
	s.Config.DBFile = filepath.Join(t.TempDir(), "gateway.db")
	if err := s.LoadSessions(); err != nil {
		t.Fatal(err)
	}
}
func sessionCall(s *Server, id, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	r.Header.Set("Content-Type", "application/json")
	if id != "" {
		r.Header.Set("X-Session-ID", id)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

const sessionResponse = `{"id":"resp_one","choices":[{"message":{"content":"private answer"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":2}}}`

func TestSessionStickyArchiveUsageAndAdminBoundaries(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Session-ID") != "" {
			t.Error("gateway session header leaked")
		}
		mu.Lock()
		keys = append(keys, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Session-ID", "upstream-must-not-replace")
		io.WriteString(w, sessionResponse)
	}), 2)
	enableSessions(t, s)
	logs := captureLogs(s)
	body := `{"model":"m","messages":[{"role":"user","content":"private question"}]}`
	for _, id := range []string{"chat1", "chat1", "chat2", "chat1"} {
		w := sessionCall(s, id, "/v1/chat/completions", body)
		if w.Code != 200 || w.Header().Get("X-Session-ID") != id {
			t.Fatalf("response: %d %s", w.Code, w.Body)
		}
	}
	if strings.Contains(logs.String(), "private question") || strings.Contains(logs.String(), "private answer") || !strings.Contains(logs.String(), `"session_id":"chat1"`) {
		t.Fatal("session logs must contain correlation without message bodies")
	}
	if strings.Join(keys, ",") != "Bearer upstream-secret-0,Bearer upstream-secret-0,Bearer upstream-secret-1,Bearer upstream-secret-0" {
		t.Fatalf("not sticky %v", keys)
	}
	sum, rows, total, err := s.Sessions.Detail("chat1", 0, 30)
	if err != nil || total != 3 || sum.Usage.Total != 30 || sum.Usage.Input != 21 || sum.Usage.Cached != 6 {
		t.Fatalf("bad usage %+v %v", sum, err)
	}
	if !rows[0].UsageComplete || rows[0].UnknownAttempts != 0 || !rows[0].Response.Complete {
		t.Fatalf("bad row %+v", rows[0])
	}
	base := "/admin/v1/sessions/chat1/requests/" + rows[0].ID
	for _, path := range []string{"/admin/v1/sessions", "/admin/v1/sessions/chat1", base, base + "/request", base + "/response"} {
		w := call(s, "GET", path, "")
		if w.Code != 401 {
			t.Fatalf("client key accepted for %s", path)
		}
	}
	w := adminCall(s, "GET", base+"/request", "")
	if w.Code != 200 || w.Body.String() != body || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("request download %d %s", w.Code, w.Body)
	}
	w = adminCall(s, "GET", base+"/response", "")
	if w.Code != 200 || w.Body.String() != sessionResponse {
		t.Fatal("response changed")
	}
	w = adminCall(s, "GET", base, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "private question") {
		t.Fatal("preview missing")
	}
	w = adminCall(s, "GET", "/admin/v1/sessions?limit=1", "")
	var page struct {
		Data  []sessions.Summary
		Total int
	}
	json.Unmarshal(w.Body.Bytes(), &page)
	if w.Code != 200 || page.Total != 2 || len(page.Data) != 1 {
		t.Fatal("pagination")
	}
	for _, query := range []string{"?offset=-1", "?limit=101", "?offset=x", "?archived=x"} {
		if w = adminCall(s, "GET", "/admin/v1/sessions"+query, ""); w.Code != 400 {
			t.Fatal("invalid filter accepted")
		}
	}
	w = adminCall(s, "PUT", "/admin/v1/sessions/chat1", `{"name":"会话 A","archived":true}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = adminCall(s, "GET", "/admin/v1/sessions?archived=true&q=会话", "")
	json.Unmarshal(w.Body.Bytes(), &page)
	if page.Total != 1 || page.Data[0].Name != "会话 A" {
		t.Fatal("archive missing")
	}
	w = adminCall(s, "GET", "/admin/v1/sessions/chat2/requests/"+rows[0].ID+"/response", "")
	if w.Code != 404 {
		t.Fatal("cross-session data leaked")
	}
	before := total
	w = sessionCall(s, "../bad", "/v1/chat/completions", body)
	if w.Code != 400 {
		t.Fatal("invalid session accepted")
	}
	_, _, after, _ := s.Sessions.Detail("chat1", 0, 30)
	if before != after {
		t.Fatal("invalid request poisoned session")
	}
	w = sessionCall(s, "", "/v1/chat/completions", body)
	if !strings.HasPrefix(w.Header().Get("X-Session-ID"), "sess-") {
		t.Fatal("automatic session missing")
	}
	// Preferred account and full records survive a gateway restart.
	store, err := sessions.Open(s.Config.DBFile, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Sessions = store
	if store.PreferredAccount("chat1", "m") != "a0" {
		t.Fatal("binding lost")
	}
}
func TestSessionSafeFailoverAndAttemptAccounting(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer upstream-secret-0" {
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"type":"rate_limit_error"},"usage":{"prompt_tokens":2,"completion_tokens":0,"total_tokens":2}}`)
			return
		}
		io.WriteString(w, sessionResponse)
	}), 2)
	enableSessions(t, s)
	w := sessionCall(s, "retry-chat", "/v1/chat/completions", `{"model":"m"}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	sum, rows, _, _ := s.Sessions.Detail("retry-chat", 0, 30)
	if len(rows[0].Attempts) != 2 || !rows[0].Attempts[0].Retry || rows[0].Usage.Total != 12 || sum.Usage.Total != 12 || !rows[0].UsageComplete || sum.PreferredAccounts["m"] != "a1" {
		t.Fatalf("bad retry accounting %+v %+v", sum, rows)
	}
	if s.Pool.Snapshot()[0].Stats.Total != 2 {
		t.Fatal("reported rejected usage lost")
	}
	// Unknown usage remains separate from a known zero or known total.
	s.Pool.SetEnabled("a1", false)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	w = contextCall(s, ctx, "POST", "/v1/chat/completions", `{"model":"m"}`, map[string]string{"X-Session-ID": "retry-chat"})
	if w.Code != 504 {
		t.Fatal("cooling account did not wait until deadline")
	}
	_, rows, _, _ = s.Sessions.Detail("retry-chat", 0, 30)
	if len(rows[0].Attempts) != 0 || rows[0].Outcome != "error" || rows[0].Failure.Reason != "pool_wait_timeout" || rows[0].PoolWaitMS == 0 {
		t.Fatal("gateway failure not archived")
	}
}
func TestStatefulContinuationNeverSwitchesAccounts(t *testing.T) {
	var calls atomic.Int32
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sessionResponse)
	}), 2)
	enableSessions(t, s)
	if w := sessionCall(s, "state-chat", "/v1/responses", `{"model":"m","input":"hello"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	// Resolved previous_response_id associates the next request even without a header.
	w := sessionCall(s, "", "/v1/responses", `{"model":"m","previous_response_id":"resp_one","input":"next"}`)
	if w.Code != 200 || w.Header().Get("X-Session-ID") != "state-chat" {
		t.Fatalf("continuation not associated: %d %s", w.Code, w.Body)
	}
	s.Pool.SetEnabled("a0", false)
	w = sessionCall(s, "state-chat", "/v1/responses", `{"model":"m","previous_response_id":"resp_one","input":"next"}`)
	if w.Code != 503 || calls.Load() != 2 {
		t.Fatal("stateful request switched accounts")
	}
	w = sessionCall(s, "other-chat", "/v1/responses", `{"model":"m","previous_response_id":"resp_one","input":"next"}`)
	if w.Code != 409 {
		t.Fatal("session conflict accepted")
	}
	w = sessionCall(s, "new-chat", "/v1/responses", `{"model":"m","previous_response_id":"unknown"}`)
	if w.Code != 409 {
		t.Fatal("unknown continuation guessed")
	}
	// A full-history request can switch away from an unavailable preferred account.
	w = sessionCall(s, "state-chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"full history"}]}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if s.Sessions.PreferredAccount("state-chat", "m") != "a1" {
		t.Fatal("safe failover did not update binding")
	}
}
func TestStatefulRateLimitRetriesOnOwningAccount(t *testing.T) {
	var reject atomic.Bool
	var calls atomic.Int32
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer upstream-secret-0" {
			t.Error("stateful request switched accounts")
		}
		if reject.Swap(false) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
			return
		}
		io.WriteString(w, sessionResponse)
	}), 2)
	enableSessions(t, s)
	sessionCall(s, "state", "/v1/responses", `{"model":"m","input":"first"}`)
	reject.Store(true)
	w := sessionCall(s, "state", "/v1/responses", `{"model":"m","previous_response_id":"resp_one"}`)
	if w.Code != 200 || calls.Load() != 3 {
		t.Fatal("stateful explicit rate limit was not retried on the same account")
	}
}
func TestStreamingSessionArchiveAndIncompleteResponse(t *testing.T) {
	for _, complete := range []bool{true, false} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			wire := "data: {\"id\":\"stream-id\",\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":4,\"total_tokens\":15}}\n\n"
			if complete {
				wire += "data: [DONE]\n\n"
			}
			s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, wire)
				w.(http.Flusher).Flush()
			}), 1)
			enableSessions(t, s)
			func() {
				defer func() {
					p := recover()
					if complete && p != nil {
						t.Fatalf("unexpected panic %v", p)
					}
					if !complete && p != http.ErrAbortHandler {
						t.Fatalf("truncated stream not aborted %v", p)
					}
				}()
				w := sessionCall(s, "stream", "/v1/chat/completions", `{"model":"m","stream":true}`)
				if complete && w.Body.String() != wire {
					t.Fatal("stream changed")
				}
			}()
			sum, rows, _, _ := s.Sessions.Detail("stream", 0, 30)
			r := rows[0]
			if r.Usage.Total != 15 || sum.Usage.Total != 15 || r.Response.Complete != complete || r.ResponseID != "stream-id" {
				t.Fatalf("bad streaming capture %+v", r)
			}
			body, err := s.Sessions.ReadBody("stream", r.ID, "response")
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(body)
			body.Close()
			if err != nil || string(got) != wire {
				t.Fatal("stream archive not exact")
			}
		})
	}
}
func TestConcurrentFirstRequestsShareBinding(t *testing.T) {
	var mu sync.Mutex
	keys := map[string]int{}
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys[r.Header.Get("Authorization")]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sessionResponse)
	}), 2)
	enableSessions(t, s)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := sessionCall(s, "parallel", "/v1/chat/completions", `{"model":"m"}`)
			if w.Code != 200 {
				t.Errorf("request %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if len(keys) != 1 {
		t.Fatal("racing initial selection broke binding")
	}
	sum, _, _, _ := s.Sessions.Detail("parallel", 0, 30)
	if sum.Requests != 6 || sum.Usage.Total != 60 {
		t.Fatal("concurrent aggregation lost updates")
	}
}
