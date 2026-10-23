package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// onlyDoneReadable blocks like a stalled upstream until its request context
// ends, then reports that context error as the body-read failure.
type onlyDoneReadable struct{ ctx context.Context }

func (b onlyDoneReadable) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b onlyDoneReadable) Close() error             { return nil }

func TestStreamStopDiagnosticSeparatesClientUpstreamAndDeadline(t *testing.T) {
	clientCtx, cancelClient := context.WithCancel(context.Background())
	cancelClient()
	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	aliveCtx := context.Background()

	for _, tt := range []struct {
		name            string
		requestCtx      context.Context
		upstreamCtx     context.Context
		wantLayer       string
		wantReason      string
		wantExplanation string
	}{
		{"client-gone", clientCtx, deadlineCtx, "client", "client_cancelled", "client"},
		{"gateway-deadline", aliveCtx, deadlineCtx, "timeout", "upstream_timeout", "deadline"},
		{"upstream-stop", aliveCtx, aliveCtx, "stream", "response_incomplete", "upstream"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(tt.requestCtx)
			d := streamStopDiagnostic(r, tt.upstreamCtx)
			if d.Layer != tt.wantLayer || d.Reason != tt.wantReason {
				t.Fatalf("layer=%s reason=%s want %s/%s", d.Layer, d.Reason, tt.wantLayer, tt.wantReason)
			}
		})
	}
}

func TestStreamErrorClass(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"clean-close", nil, "upstream_closed"},
		{"deadline", context.DeadlineExceeded, "deadline_exceeded"},
		{"cancel", context.Canceled, "canceled"},
		{"goaway", errString("http2: server sent GOAWAY"), "http2_goaway"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := streamErrorClass(tt.err); got != tt.want {
				t.Fatalf("class=%q want %q", got, tt.want)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// A gateway request deadline that fires mid-stream used to be archived as
// "response_incomplete", which points triage at the upstream even though the
// gateway stopped the stream. It must be labeled as a timeout and keep the
// concrete read error in the log.
func TestGatewayDeadlineMidStreamIsReportedAsTimeout(t *testing.T) {
	s, _ := newTestGateway(t, http.NotFoundHandler(), 1)
	s.Config.RequestTimeoutSeconds = 1
	enableSessions(t, s)
	logs := captureLogs(s)
	s.client.Transport = streamTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       onlyDoneReadable{ctx: r.Context()},
			Request:    r,
		}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := httptest.NewRecorder()
	r := streamRequest(s, ctx, "/v1/chat/completions", `{"model":"m","stream":true}`, map[string]string{"X-Session-ID": "deadline"})
	if aborted := serveStream(s, w, r); aborted != http.ErrAbortHandler {
		t.Fatalf("stalled stream was not aborted: %v", aborted)
	}
	_, rows, _, err := s.Sessions.Detail("deadline", 0, 30)
	if err != nil || len(rows) != 1 {
		t.Fatalf("deadline request not archived: %+v %v", rows, err)
	}
	if rows[0].Failure.Layer != "timeout" || rows[0].Failure.Reason != "upstream_timeout" {
		t.Fatalf("gateway deadline mislabeled: %+v", rows[0].Failure)
	}
	found := false
	for _, record := range logRecords(t, logs) {
		if record["event"] != "stream_copy_failed" {
			continue
		}
		found = true
		if record["read_error"] != "deadline_exceeded" || record["reason"] != "upstream_timeout" {
			t.Fatalf("stream failure log lost its cause: %+v", record)
		}
	}
	if !found {
		t.Fatal("no stream_copy_failed log for a truncated response")
	}
}
