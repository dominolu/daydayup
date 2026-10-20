package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"daydayup/internal/sessions"
)

type streamTransport func(*http.Request) (*http.Response, error)

func (f streamTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A real streaming server may leave the HTTP body open after its terminal event.
// The old proxy performed an extra Read and saw cancellation after delivery.
type scriptedStream struct {
	chunks    []string
	tailErr   error
	extraRead int
	closed    bool
}

func (s *scriptedStream) Read(p []byte) (int, error) {
	if len(s.chunks) == 0 {
		s.extraRead++
		return 0, s.tailErr
	}
	time.Sleep(3 * time.Millisecond)
	n := copy(p, s.chunks[0])
	s.chunks[0] = s.chunks[0][n:]
	if len(s.chunks[0]) == 0 {
		s.chunks = s.chunks[1:]
	}
	return n, nil
}
func (s *scriptedStream) Close() error { s.closed = true; return nil }

type lifecycleWriter struct {
	*httptest.ResponseRecorder
	terminal  string
	cancel    context.CancelFunc
	writeFail bool
	short     bool
	flushFail bool
}

func (w *lifecycleWriter) Write(p []byte) (int, error) {
	if (w.writeFail || w.short) && bytes.Contains(p, []byte(w.terminal)) {
		n, _ := w.ResponseRecorder.Write(p[:len(p)/2])
		if w.writeFail {
			return n, io.ErrClosedPipe
		}
		return n, nil
	}
	return w.ResponseRecorder.Write(p)
}
func (w *lifecycleWriter) FlushError() error {
	w.ResponseRecorder.Flush()
	if strings.HasSuffix(w.Body.String(), w.terminal) {
		if w.cancel != nil {
			w.cancel()
		}
		if w.flushFail {
			return io.ErrClosedPipe
		}
	}
	return nil
}

func streamRequest(s *Server, ctx context.Context, path, body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func serveStream(s *Server, w http.ResponseWriter, r *http.Request) (aborted any) {
	defer func() { aborted = recover() }()
	s.ServeHTTP(w, r)
	return nil
}

func useStream(s *Server, body io.ReadCloser) {
	s.client.Transport = streamTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
	})
}

func TestTerminalDeliveryBeforeClientCancellationIsSuccessful(t *testing.T) {
	for _, tc := range []struct {
		name, path, terminal string
		chunks               []string
	}{
		{"chat", "/v1/chat/completions", "data: [DONE]\r\n\r\n", []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\" there\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n",
			"data: [DONE]\r\n", "\r\n",
		}},
		{"messages", "/v1/messages", "data: {\"type\":\"message_stop\"}\n\n", []string{
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n",
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" there\"}}\n\n",
			"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n",
			"data: {\"type\":\"message_stop\"}\n\n",
		}},
		{"responses", "/v1/responses", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n", []string{
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n",
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\" there\"}\n\n",
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestGateway(t, http.NotFoundHandler(), 1)
			enableSessions(t, s)
			logs := captureLogs(s)
			body := &scriptedStream{chunks: append([]string(nil), tc.chunks...), tailErr: context.Canceled}
			useStream(s, body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := &lifecycleWriter{ResponseRecorder: httptest.NewRecorder(), terminal: tc.terminal, cancel: cancel}
			r := streamRequest(s, ctx, tc.path, `{"model":"m","stream":true}`, map[string]string{"X-Session-ID": "complete"})
			if aborted := serveStream(s, w, r); aborted != nil {
				t.Fatalf("complete protocol was aborted: %v", aborted)
			}
			if ctx.Err() == nil || body.extraRead != 0 || !body.closed || w.Code != 200 || w.Body.String() != strings.Join(tc.chunks, "") {
				t.Fatalf("read past completion or changed bytes: extra reads=%d closed=%v", body.extraRead, body.closed)
			}
			sum, rows, _, _ := s.Sessions.Detail("complete", 0, 30)
			if sum.Failures != 0 || rows[0].Outcome != "success" || !rows[0].Response.Complete || !rows[0].UsageComplete || sum.Usage.Total != 10 || sum.Performance.TPS == nil || sum.Performance.TPSSamples != 1 || s.Pool.Snapshot()[0].Stats.Failures != 0 {
				t.Fatalf("late cancellation corrupted accounting: %+v %+v", sum, rows)
			}
			for _, record := range logRecords(t, logs) {
				if record["outcome"] != "success" || record["sse_terminal_received"] != true || record["sse_terminal_forwarded"] != true {
					t.Fatalf("false cancellation in log: %+v", record)
				}
			}
			reopened, err := sessions.Open(s.Config.DBFile, "")
			if err != nil {
				t.Fatal(err)
			}
			restored, _, _, _ := reopened.Detail("complete", 0, 30)
			if restored.Failures != 0 || restored.Performance.TPS == nil || restored.Usage.Total != 10 {
				t.Fatal("completion or TPS lost on restart")
			}
		})
	}
}

func TestMissingTerminalAndDeliveryFailuresRemainIncomplete(t *testing.T) {
	const delta = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"
	const final = "data: {\"choices\":[{\"delta\":{\"content\":\" there\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n"
	const done = "data: [DONE]\n\n"
	for _, name := range []string{"eof-before-terminal", "unframed-terminal", "client-cancelled-early", "write-failure", "short-write", "flush-failure"} {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestGateway(t, http.NotFoundHandler(), 2)
			enableSessions(t, s)
			logs := captureLogs(s)
			body := &scriptedStream{chunks: []string{delta, final}, tailErr: io.EOF}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := &lifecycleWriter{ResponseRecorder: httptest.NewRecorder(), terminal: done}
			switch name {
			case "unframed-terminal":
				body.chunks = append(body.chunks, "data: [DONE]\n")
			case "client-cancelled-early":
				body.tailErr, w.terminal, w.cancel = context.Canceled, final, cancel
			case "write-failure", "short-write", "flush-failure":
				body.chunks = append(body.chunks, done)
				w.writeFail, w.short, w.flushFail = name == "write-failure", name == "short-write", name == "flush-failure"
			}
			useStream(s, body)
			r := streamRequest(s, ctx, "/v1/chat/completions", `{"model":"m","stream":true}`, map[string]string{"X-Session-ID": "incomplete"})
			if aborted := serveStream(s, w, r); aborted != http.ErrAbortHandler {
				t.Fatalf("partial delivery was not aborted: %v", aborted)
			}
			sum, rows, _, _ := s.Sessions.Detail("incomplete", 0, 30)
			if sum.Failures != 1 || rows[0].Outcome != "error" || rows[0].Response.Complete || rows[0].UsageComplete || sum.Performance.TPS != nil || sum.Usage.Total != 10 || s.Pool.Snapshot()[0].Stats.Failures != 1 {
				t.Fatalf("incomplete response claimed success: %+v %+v", sum, rows)
			}
			if s.Pool.Snapshot()[1].Stats.Requests != 0 || !body.closed {
				t.Fatal("partial stream replayed or upstream body leaked")
			}
			for _, record := range logRecords(t, logs) {
				if record["outcome"] != "error" || record["sse_terminal_forwarded"] != false {
					t.Fatal("delivery failure was hidden in logs")
				}
			}
		})
	}
}

func TestTerminalResponseErrorIsNotSuccess(t *testing.T) {
	for _, name := range []string{"response.failed", "response.incomplete"} {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestGateway(t, http.NotFoundHandler(), 1)
			enableSessions(t, s)
			wire := "data: {\"type\":\"" + name + "\",\"response\":{\"error\":{\"type\":\"server_error\"},\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n"
			if name == "response.incomplete" {
				wire = "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n"
			}
			body := &scriptedStream{chunks: []string{wire}, tailErr: io.ErrUnexpectedEOF}
			useStream(s, body)
			w := sessionCall(s, "upstream-failure", "/v1/responses", `{"model":"m","stream":true}`)
			sum, rows, _, _ := s.Sessions.Detail("upstream-failure", 0, 30)
			if w.Code != 200 || sum.Failures != 1 || rows[0].Outcome != "error" || rows[0].Failure.Layer != "upstream" || !rows[0].Response.Complete || sum.Performance.TPS != nil || body.extraRead != 0 {
				t.Fatal("terminal upstream failure treated as success", rows)
			}
		})
	}
}

func TestHeaderlessToolContinuationAfterTerminalClose(t *testing.T) {
	s, _ := newTestGateway(t, http.NotFoundHandler(), 2)
	enableSessions(t, s)
	up := httptest.NewRecorder()
	toolUpstream(up, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(toolRequest)))
	body := &scriptedStream{chunks: []string{up.Body.String()}, tailErr: context.Canceled}
	useStream(s, body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &lifecycleWriter{ResponseRecorder: httptest.NewRecorder(), terminal: "data: [DONE]\n\n", cancel: cancel}
	r := streamRequest(s, ctx, "/v1/chat/completions", toolRequest, map[string]string{"User-Agent": "deepseek-harness/0.2.0"})
	if aborted := serveStream(s, w, r); aborted != nil {
		t.Fatal(aborted)
	}
	sid := w.Header().Get("X-Session-ID")
	store, err := sessions.Open(s.Config.DBFile, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Sessions = store
	s.client.Transport = streamTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(sessionResponse)), Request: r}, nil
	})
	second := identityCall(s, "/v1/chat/completions", toolFollowup, map[string]string{"User-Agent": "deepseek-harness/0.2.0"})
	sum, rows, total, _ := store.Detail(sid, 0, 30)
	if second.Code != 200 || second.Header().Get("X-Session-ID") != sid || second.Header().Get("X-Session-Source") != "tool_continuation" || total != 2 || sum.Failures != 0 || sum.Usage.Total != 20 || rows[0].AccountID != rows[1].AccountID {
		t.Fatalf("terminal close broke session continuation: %+v %+v", sum, rows)
	}
}

func TestTitleAdoptedAfterTerminalClose(t *testing.T) {
	s, _ := newTestGateway(t, http.NotFoundHandler(), 1)
	enableSessions(t, s)
	s.client.Transport = streamTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(sessionResponse)), Request: r}, nil
	})
	if w := sessionCall(s, "naming", "/v1/chat/completions", mainBody("修复流结束状态")); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	const wire = "data: {\"choices\":[{\"delta\":{\"content\":\"修复流结束状态\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n"
	body := &scriptedStream{chunks: []string{wire}, tailErr: context.Canceled}
	useStream(s, body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &lifecycleWriter{ResponseRecorder: httptest.NewRecorder(), terminal: "data: [DONE]\n\n", cancel: cancel}
	r := streamRequest(s, ctx, "/v1/chat/completions", titleBody("修复流结束状态", true), map[string]string{"X-Session-ID": "naming"})
	if aborted := serveStream(s, w, r); aborted != nil {
		t.Fatal(aborted)
	}
	sum, rows, _, _ := s.Sessions.Detail("naming", 0, 30)
	if sum.Name != "修复流结束状态" || sum.NameInfo.Source != "model_title" || sum.Failures != 0 || rows[0].Title.Status != "applied" || sum.Usage.Total != 20 {
		t.Fatalf("terminal close rejected valid title: %+v %+v", sum, rows)
	}
}

func TestSSETerminalDoesNotWaitForUpstreamHTTPEOF(t *testing.T) {
	const wire = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n"
	closed := make(chan struct{})
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, wire)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // Never return HTTP EOF until the gateway releases us.
		close(closed)
	}), 1)
	gw := httptest.NewServer(s)
	defer gw.Close()
	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != wire {
		t.Fatal("gateway waited for HTTP EOF after protocol completion", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream slot not released at terminal event")
	}
	state := s.Pool.Snapshot()[0]
	if state.InFlight != 0 || state.Stats.Failures != 0 || state.Stats.Total != 10 {
		t.Fatal("terminal close damaged pool accounting", state)
	}
}
