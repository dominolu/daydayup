package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func captureLogs(s *Server) *bytes.Buffer {
	var output bytes.Buffer
	s.logger = slog.New(slog.NewJSONHandler(&output, nil))
	return &output
}

func logRecords(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err == io.EOF {
			return records
		} else if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
}

func TestUpstreamErrorsAreTraceableWithoutChangingResponses(t *testing.T) {
	for _, tt := range []struct {
		name, typ, code, message, reason string
		status                           int
	}{
		{"model-permission", "permission_denied_error", "7", "model is not available in the current token plan", "model_not_in_token_plan", 403},
		{"model-rate-limit", "rate_limit_error", "RateLimitExceeded.EndpointTPMExceeded", "inference exceeds tpm/rpm limit", "model_rate_limit", 429},
		{"upstream-auth", "authentication_error", "invalid_api_key", "private message upstream-secret-0 PRIVATE_PROMPT", "upstream_auth_failed", 401},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": tt.typ, "code": tt.code, "message": tt.message}, "debug": "PRIVATE_PROMPT"})
			s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-ID", "upstream-request-123")
				w.WriteHeader(tt.status)
				_, _ = w.Write(body)
			}), 1)
			logs := captureLogs(s)
			// Arbitrary writes are forwarded once, including their raw 429.
			request := httptest.NewRequest("POST", "/v1/files?api_key=PRIVATE_QUERY", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`))
			request.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Cookie", "session=PRIVATE_COOKIE")
			request.Header.Set("X-Request-ID", "untrusted-client-id")
			response := httptest.NewRecorder()
			s.ServeHTTP(response, request)
			if response.Code != tt.status || !bytes.Equal(response.Body.Bytes(), body) {
				t.Fatal("logging changed the upstream response")
			}
			records := logRecords(t, logs)
			if len(records) != 2 || records[0]["event"] != "upstream_attempt" || records[1]["event"] != "request_completed" {
				t.Fatal("missing attempt or completion log")
			}
			for _, record := range records {
				if record["request_id"] != response.Header().Get("X-Request-ID") || record["request_id"] == "untrusted-client-id" || record["upstream_request_id"] != "upstream-request-123" {
					t.Fatal("request correlation was lost")
				}
				if record["status"] != float64(tt.status) || record["reason"] != tt.reason || record["error_layer"] != "upstream" || record["model"] != "deepseek-v4.1-flash" || record["account_id"] != "a0" || record["error_code"] != tt.code || record["error_type"] != tt.typ {
					t.Fatalf("inaccurate diagnostic: %+v", record)
				}
			}
			for _, secret := range []string{s.Config.GatewayKey, s.Config.AdminKey, s.Config.Accounts[0].APIKey, "PRIVATE_PROMPT", "PRIVATE_QUERY", "PRIVATE_COOKIE"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("request data or credentials leaked into logs")
				}
			}
		})
	}
}

func TestGatewayAuthFailureIsLoggedBeforeAnyUpstreamAttempt(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unauthorized request reached upstream") }), 1)
	logs := captureLogs(s)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	r.Header.Set("Authorization", "Bearer incorrect-client-credential")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	records := logRecords(t, logs)
	if w.Code != 401 || len(records) != 1 || records[0]["error_layer"] != "gateway_auth" || records[0]["attempts"] != float64(0) {
		t.Fatal("gateway authentication was not distinguished from upstream authentication")
	}
	if strings.Contains(logs.String(), "incorrect-client-credential") {
		t.Fatal("rejected credential was logged")
	}
}

func TestRetryLogsShareRequestIDAndFinalSuccessClearsFailure(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer upstream-secret-0" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","code":"invalid_api_key"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	}), 2)
	logs := captureLogs(s)
	w := call(s, "POST", "/v1/chat/completions", `{"model":"m"}`)
	records := logRecords(t, logs)
	if w.Code != 200 || len(records) != 3 || records[0]["retry"] != true || records[0]["attempts"] != float64(1) || records[1]["account_id"] != "a1" || records[1]["total_tokens"] != float64(5) {
		t.Fatal("retry attempts or usage were not recorded")
	}
	for _, record := range records {
		if record["request_id"] != w.Header().Get("X-Request-ID") {
			t.Fatal("retry request ID changed")
		}
	}
	final := records[2]
	if final["outcome"] != "success" || final["error_layer"] != nil || final["attempts"] != float64(2) {
		t.Fatal("final success inherited an earlier account failure")
	}
}

func TestSSEErrorIsLoggedEvenWhenHTTPStatusIsOK(t *testing.T) {
	body := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"permission_denied_error\",\"code\":\"7\",\"message\":\"model is not available in the current token plan\"}}}\n\n"
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}), 1)
	logs := captureLogs(s)
	w := call(s, "POST", "/v1/responses", `{"model":"m","stream":true}`)
	records := logRecords(t, logs)
	if w.Code != 200 || w.Body.String() != body || len(records) != 2 {
		t.Fatal("stream error logging changed the SSE response")
	}
	if records[1]["outcome"] != "error" || records[1]["status"] != float64(200) || records[1]["reason"] != "model_not_in_token_plan" {
		t.Fatal("HTTP 200 concealed the stream error")
	}
}

func TestErrorLogIsBoundedAndDoesNotLogUnrecognizedMessage(t *testing.T) {
	body := `{"error":{"type":"invalid_request_error","message":"` + strings.Repeat("PRIVATE", 12000) + `"}}`
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, body)
	}), 1)
	logs := captureLogs(s)
	w := call(s, "POST", "/v1/chat/completions", `{"model":"m"}`)
	if w.Code != 400 || w.Body.String() != body {
		t.Fatal("bounded inspection changed the error body")
	}
	if logs.Len() > 10000 || strings.Contains(logs.String(), "PRIVATE") {
		t.Fatal("unrecognized error message was logged")
	}
}

func TestLogMetadataRedactsCredentials(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "upstream-secret-0")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"type":"upstream-secret-0","code":"sk-another-secret","message":"PRIVATE"}}`)
	}), 1)
	logs := captureLogs(s)
	model := strings.Join([]string{s.Config.GatewayKey, s.Config.AdminKey, s.Config.Accounts[0].APIKey}, "|")
	body, _ := json.Marshal(map[string]string{"model": model})
	_ = call(s, "POST", "/v1/custom/"+s.Config.GatewayKey, string(body))
	for _, secret := range []string{s.Config.GatewayKey, s.Config.AdminKey, s.Config.Accounts[0].APIKey, "sk-another-secret", "PRIVATE"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("credentials leaked through diagnostic metadata")
		}
	}
}

func TestCachedModelDenialReturns403WithoutRequestingUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		var payload struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "application/json")
		if payload.Model == "denied" {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"error":{"type":"permission_denied_error","code":"7","message":"model is not available in the current token plan"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":1}}`)
	}), 1)
	logs := captureLogs(s)
	first := call(s, "POST", "/v1/chat/completions", `{"model":"denied"}`)
	if first.Code != 403 || !strings.Contains(first.Body.String(), `"code":"7"`) {
		t.Fatal("initial upstream permission error was changed")
	}
	cached := call(s, "POST", "/v1/chat/completions", `{"model":"denied"}`)
	if cached.Code != 403 || !strings.Contains(cached.Body.String(), `"code":"model_not_in_token_plan"`) || cached.Header().Get("Retry-After") != "" || upstreamCalls.Load() != 1 {
		t.Fatal("cached permission denial became a retryable pool-unavailable error")
	}
	var record map[string]any
	for _, item := range logRecords(t, logs) {
		if item["request_id"] == cached.Header().Get("X-Request-ID") {
			record = item
		}
	}
	rejections, ok := record["pool_rejections"].([]any)
	if !ok || len(rejections) != 1 || rejections[0].(map[string]any)["reason"] != "model_not_in_token_plan" || record["attempts"] != float64(0) {
		t.Fatal("cached rejection lacks an account-level explanation in the log")
	}
	allowed := call(s, "POST", "/v1/chat/completions", `{"model":"allowed"}`)
	if allowed.Code != 200 || upstreamCalls.Load() != 2 {
		t.Fatal("permission cache affected an available model")
	}
}

func TestModelCooldownWaitsAndExplainsItsCause(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("cooled-down model reached upstream") }), 1)
	logs := captureLogs(s)
	s.Pool.ThrottleModel("a0", "limited", time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	w := contextCall(s, ctx, "POST", "/v1/chat/completions", `{"model":"limited"}`, nil)
	if w.Code != 504 || !strings.Contains(w.Body.String(), "pool_wait_timeout") {
		t.Fatal("temporary cooldown did not wait until the deadline")
	}
	records := logRecords(t, logs)
	if len(records) != 3 || records[0]["event"] != "pool_wait_started" || records[0]["pool_rejections"] == nil || records[2]["pool_wait_ms"] == nil {
		t.Fatal("cooldown rejection was not logged")
	}
}
