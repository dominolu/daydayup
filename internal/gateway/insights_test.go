package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"safuno/internal/sessions"
)

func TestToolReportAPIBoundariesAndSessionMetrics(t *testing.T) {
	var upstreamCalls atomic.Int32
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sessionResponse)
	}), 1)
	enableSessions(t, s)
	w := sessionCall(s, "insight-session", "/v1/chat/completions", `{"model":"m"}`)
	requestID := w.Header().Get("X-Request-ID")
	path := "/gateway/v1/sessions/insight-session/tool-events"
	payload := fmt.Sprintf(`{"id":"call_1","request_id":%q,"name":"search","duration_ms":300,"status":"success"}`, requestID)
	for _, tc := range []struct {
		body   string
		status int
	}{{payload, 201}, {payload, 200}, {strings.Replace(payload, "300", "400", 1), 409}, {`{"id":"bad","request_id":"unknown","duration_ms":1}`, 404}, {`{"id":"bad","request_id":"unknown"}`, 400}, {strings.TrimSuffix(payload, "}") + `,"source":"gateway_measured"}`, 400}} {
		w = call(s, "POST", path, tc.body)
		if w.Code != tc.status {
			t.Fatalf("tool API %d want %d: %s", w.Code, tc.status, w.Body)
		}
	}
	if w = adminCall(s, "POST", path, payload); w.Code != 401 {
		t.Fatal("admin credential used as client credential")
	}
	if w = call(s, "GET", "/admin/v1/sessions/insight-session/tool-events", ""); w.Code != 401 {
		t.Fatal("client read admin tool details")
	}
	w = adminCall(s, "GET", "/admin/v1/sessions/insight-session", "")
	var detail struct {
		Data  sessions.Summary
		Tools []sessions.ToolEvent `json:"tool_events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if *detail.Data.Performance.ToolTimeMS != 300 || detail.Data.Performance.ToolCalls != 1 || detail.Data.Requests != 1 || detail.Data.Usage.Total != 10 || len(detail.Tools) != 1 || upstreamCalls.Load() != 1 {
		t.Fatalf("tool report changed model accounting %+v", detail)
	}
	if detail.Data.Performance.TTFTMS != nil || detail.Data.Performance.TPS != nil {
		t.Fatal("non-stream timing invented")
	}
	if detail.Data.TokenDetails.CacheHitRate == nil || *detail.Data.TokenDetails.UncachedInput != 5 {
		t.Fatal("cache breakdown missing")
	}
	w = adminCall(s, "GET", "/admin/v1/sessions/insight-session/requests/"+requestID, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"tool_time_source":"client_reported"`) {
		t.Fatal("request telemetry missing")
	}
	w = adminCall(s, "GET", "/admin/v1/sessions/insight-session/tool-events?limit=1", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatal("tool pagination missing")
	}
	w = call(s, "POST", "/gateway/v1/sessions/other/tool-events", payload)
	if w.Code != 404 {
		t.Fatal("cross-session association accepted")
	}
}
func TestStreamingInsightsAreSavedWithoutChangingSSE(t *testing.T) {
	const first = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"
	const last = "data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n"
	const tail = "data: {\"usage\":{\"prompt_tokens\":25092,\"completion_tokens\":2188,\"total_tokens\":27280,\"prompt_tokens_details\":{\"cached_tokens\":15360}}}\n\ndata: [DONE]\n\n"
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": heartbeat\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		time.Sleep(40 * time.Millisecond)
		io.WriteString(w, last)
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		io.WriteString(w, tail)
		w.(http.Flusher).Flush()
	}), 1)
	enableSessions(t, s)
	w := sessionCall(s, "stream-insights", "/v1/chat/completions", `{"model":"m","stream":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), first+last+tail) {
		t.Fatal("stream changed")
	}
	sum, rows, _, _ := s.Sessions.Detail("stream-insights", 0, 30)
	p := sum.Performance
	if p.TTFTMS == nil || *p.TTFTMS < 15 || p.TPS == nil || p.TPSSamples != 1 || p.TTFTSamples != 1 || p.GenerationMS < 30 || p.GenerationTokens != 2188 {
		t.Fatalf("stream insights missing %+v", p)
	}
	if sum.TokenDetails.UncachedInput == nil || *sum.TokenDetails.UncachedInput != 9732 || sum.TokenDetails.CacheHitRate == nil || int(*sum.TokenDetails.CacheHitRate) != 61 {
		t.Fatalf("wrong token breakdown %+v", sum.TokenDetails)
	}
	if rows[0].Attempts[0].Timing == nil {
		t.Fatal("per-attempt timing not saved")
	}
	reopened, err := sessions.Open(s.Config.DBFile, "")
	if err != nil {
		t.Fatal(err)
	}
	restored, _, _, _ := reopened.Detail("stream-insights", 0, 30)
	if restored.Performance.TPS == nil || *restored.Performance.TPS != *p.TPS || *restored.Performance.TTFTMS != *p.TTFTMS {
		t.Fatal("performance lost on restart")
	}
}
