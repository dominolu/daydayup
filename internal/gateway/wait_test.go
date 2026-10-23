package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"daydayup/internal/pool"
)

func contextCall(s *Server, ctx context.Context, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	r.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestSingleAccountRateLimitWaitsAndArchivesEveryAttempt(t *testing.T) {
	var count atomic.Int32
	var startsMu sync.Mutex
	var starts []time.Time
	const body = `{"model":"m","messages":[{"role":"user","content":"unchanged"}],"custom":true}`
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startsMu.Lock()
		starts = append(starts, time.Now())
		startsMu.Unlock()
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Error("retry changed the request body")
		}
		w.Header().Set("Content-Type", "application/json")
		if count.Add(1) <= 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"inference exceeds tpm/rpm limit"},"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
			return
		}
		io.WriteString(w, sessionResponse)
	}), 1)
	s.Config.MaxAttempts = 1 // Rate-limit waiting is governed by the deadline.
	enableSessions(t, s)
	logs := captureLogs(s)
	w := sessionCall(s, "waiting-session", "/v1/chat/completions", body)
	if w.Code != 200 || count.Load() != 4 || w.Body.String() != sessionResponse {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, count.Load(), w.Body)
	}
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 950*time.Millisecond {
			t.Fatal("retried before Retry-After")
		}
	}
	sum, rows, total, err := s.Sessions.Detail("waiting-session", 0, 30)
	if err != nil || total != 1 || sum.Failures != 0 || len(rows[0].Attempts) != 4 || rows[0].Usage.Total != 13 || rows[0].PoolWaitMS < 2800 || rows[0].PoolWaits != 3 {
		t.Fatalf("bad wait accounting: %+v %+v %v", sum, rows, err)
	}
	if s.Pool.Snapshot()[0].InFlight != 0 || len(s.waitSlots) != 0 {
		t.Fatal("waiting leaked a reservation")
	}
	var completed map[string]any
	for _, row := range logRecords(t, logs) {
		if row["event"] == "request_completed" {
			completed = row
		}
	}
	if completed["pool_waits"] != float64(3) || completed["status"] != float64(200) {
		t.Fatal("waiting is not observable in completion logs")
	}
}

func TestCooldownWaitDeadlineAndCancellation(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancel"}[cancelClient], func(t *testing.T) {
			var count atomic.Int32
			s, _ := newTestGateway(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { count.Add(1) }), 1)
			s.Pool.ThrottleModel("a0", "m", time.Minute)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if cancelClient {
				go func() {
					time.Sleep(20 * time.Millisecond)
					cancel()
				}()
			}
			started := time.Now()
			w := contextCall(s, ctx, "POST", "/v1/chat/completions", `{"model":"m"}`, nil)
			want, code := 504, "pool_wait_timeout"
			if cancelClient {
				want, code = 499, "client_cancelled"
			}
			if w.Code != want || !strings.Contains(w.Body.String(), code) || count.Load() != 0 || time.Since(started) > time.Second || len(s.waitSlots) != 0 {
				t.Fatalf("status=%d calls=%d response=%s", w.Code, count.Load(), w.Body)
			}
		})
	}
}

func TestServerDeadlineIncludesAllRateLimitWaits(t *testing.T) {
	var count atomic.Int32
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	}), 1)
	s.Config.RequestTimeoutSeconds = 1
	started := time.Now()
	w := call(s, "POST", "/v1/chat/completions", `{"model":"m"}`)
	if w.Code != 504 || !strings.Contains(w.Body.String(), "pool_wait_timeout") || count.Load() != 1 || time.Since(started) > 2*time.Second || s.Pool.Snapshot()[0].InFlight != 0 {
		t.Fatalf("deadline was reset or upstream hammered: %d %s, calls %d", w.Code, w.Body, count.Load())
	}
}

func TestWaitingQueueIsBoundedAndReleased(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("cooldown bypassed") }), 1)
	s.waitSlots = make(chan struct{}, 1)
	s.Pool.ThrottleModel("a0", "m", time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- contextCall(s, ctx, "POST", "/v1/chat/completions", `{"model":"m"}`, nil) }()
	deadline := time.Now().Add(time.Second)
	for len(s.waitSlots) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.waitSlots) != 1 {
		t.Fatal("request did not enter queue")
	}
	w := call(s, "POST", "/v1/chat/completions", `{"model":"m"}`)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "pool_queue_full") {
		t.Fatal("waiting queue overflow was not bounded", w.Code, w.Body)
	}
	cancel()
	select {
	case w = <-done:
		if w.Code != 499 || len(s.waitSlots) != 0 {
			t.Fatal("cancelled queue entry leaked")
		}
	case <-time.After(time.Second):
		t.Fatal("waiting did not stop on cancellation")
	}
}

func TestConcurrencyWaitWakesWhenSlotReleased(t *testing.T) {
	var count atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			close(started)
			<-release
		}
		io.WriteString(w, `{}`)
	}), 1)
	s.Config.Accounts[0].MaxInFlight = 1
	s.Pool = pool.New(s.Config.Accounts)
	done := make(chan *httptest.ResponseRecorder, 2)
	go func() { done <- call(s, "POST", "/v1/chat/completions", `{"model":"m"}`) }()
	<-started
	go func() { done <- call(s, "POST", "/v1/chat/completions", `{"model":"m"}`) }()
	deadline := time.Now().Add(time.Second)
	for len(s.waitSlots) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if count.Load() != 1 || len(s.waitSlots) != 1 {
		t.Fatal("concurrency request did not wait")
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case w := <-done:
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
		case <-time.After(time.Second):
			t.Fatal("capacity release did not wake waiter")
		}
	}
}

func TestRateLimitDelayHonorsUpstreamAndBacksOff(t *testing.T) {
	if rateLimitDelay("0", 1) < time.Second || rateLimitDelay("5", 4) != 5*time.Second || rateLimitDelay("", 1) != 30*time.Second || rateLimitDelay("bad", 2) != time.Minute || rateLimitDelay("", 99) != 2*time.Minute {
		t.Fatal("bad rate-limit pacing")
	}
}

func TestConcurrentCooldownWaitersArePaced(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	var active, maxActive atomic.Int32
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		for old := maxActive.Load(); n > old && !maxActive.CompareAndSwap(old, n); old = maxActive.Load() {
		}
		defer active.Add(-1)
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		io.WriteString(w, `{}`)
	}), 1)
	s.Pool.ThrottleModel("a0", "m", 20*time.Millisecond)
	done := make(chan *httptest.ResponseRecorder, 3)
	for i := 0; i < 3; i++ {
		go func() { done <- call(s, "POST", "/v1/chat/completions", `{"model":"m"}`) }()
	}
	for i := 0; i < 3; i++ {
		w := <-done
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive.Load() != 1 || len(starts) != 3 {
		t.Fatal("recovery waiters rushed upstream together")
	}
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 950*time.Millisecond {
			t.Fatal("recovery requests were not spaced")
		}
	}
}

func TestRateLimitWaitBeforeSSEDoesNotCommitErrorResponse(t *testing.T) {
	var count atomic.Int32
	const wire = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, wire)
		w.(http.Flusher).Flush()
	}), 1)
	gw := httptest.NewServer(s)
	defer gw.Close()
	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || string(got) != wire || resp.Header.Get("Retry-After") != "" || count.Load() != 2 {
		t.Fatalf("SSE polluted by intermediate rejection: %d %q %v", resp.StatusCode, got, err)
	}
}

// Rate-limit waiting must not eat the whole generation window: once the
// dedicated budget is spent the request fails fast with a distinct reason the
// client can act on, instead of being cancelled mid-stream later.
func TestPoolWaitBudgetStopsRateLimitWaiting(t *testing.T) {
	var count atomic.Int32
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"limit"}}`)
	}), 1)
	s.Config.MaxAttempts = 1
	s.Config.PoolWaitTimeoutSeconds = 1
	// Activating the response middleware wraps the writer (model rename keeps
	// the canonical name in responses), which used to hide gateway errors from
	// the archived failure record.
	s.Config.Middleware.ModelMap = map[string]string{"alias": "m"}
	enableSessions(t, s)
	started := time.Now()
	w := sessionCall(s, "wait-budget", "/v1/chat/completions", `{"model":"m"}`)
	elapsed := time.Since(started)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "pool_wait_budget_exhausted") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("budget did not bound the wait: %s", elapsed)
	}
	if count.Load() != 1 {
		t.Fatalf("kept retrying after the budget exit: %d calls", count.Load())
	}
	if s.Pool.Snapshot()[0].InFlight != 0 || len(s.waitSlots) != 0 {
		t.Fatal("budget exit leaked a waiting slot")
	}
	_, rows, _, err := s.Sessions.Detail("wait-budget", 0, 30)
	if err != nil || len(rows) != 1 || rows[0].Failure.Reason != "pool_wait_budget_exhausted" || rows[0].Failure.Layer != "pool" || rows[0].PoolWaits != 1 || rows[0].PoolWaitMS == 0 {
		t.Fatalf("wait budget exit not archived: %+v %v", rows, err)
	}
}
