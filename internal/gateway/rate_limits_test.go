package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRateLimitDiagnosticsSurviveRetryAndArchiveReload(t *testing.T) {
	const body = `{"model":"m","max_completion_tokens":32768,"messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`
	const response = `{"usage":{"prompt_tokens":100,"completion_tokens":3,"total_tokens":103,"prompt_tokens_details":{"cached_tokens":80}}}`
	reset := time.Now().UTC().Add(time.Minute).Format(http.TimeFormat)
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Error("output-cap observation changed the upstream payload")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer upstream-secret-0" {
			w.Header().Set("Retry-After", "120")
			w.Header().Set("X-RateLimit-Limit-Tokens", "600000")
			w.Header().Add("X-RateLimit-Remaining-Requests", "0")
			w.Header().Add("X-RateLimit-Remaining-Requests", "1")
			w.Header().Set("X-RateLimit-Reset-Tokens", reset)
			w.Header().Set("X-RateLimit-Api-Key", "PRIVATE_HEADER")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"429003","message":"inference exceeds tpm/rpm limit"}}`)
			return
		}
		w.Header().Set("RateLimit-Remaining", "9")
		io.WriteString(w, response)
	}), 2)
	enableSessions(t, s)
	logs := captureLogs(s)
	w := sessionCall(s, "rate-diagnostics", "/v1/chat/completions", body)
	if w.Code != 200 || w.Body.String() != response || w.Header().Get("RateLimit-Remaining") != "9" || w.Header().Get("Retry-After") != "" {
		t.Fatal("retry telemetry changed the final response")
	}
	records := logRecords(t, logs)
	if len(records) != 3 {
		t.Fatalf("expected two attempts and one completion, got %d", len(records))
	}
	first, second := records[0], records[1]
	headers := first["rate_limit_headers"].(map[string]any)
	if !reflect.DeepEqual(headers["Retry-After"], []any{"120"}) || !reflect.DeepEqual(headers["X-Ratelimit-Limit-Tokens"], []any{"600000"}) || !reflect.DeepEqual(headers["X-Ratelimit-Remaining-Requests"], []any{"0", "1"}) || !reflect.DeepEqual(headers["X-Ratelimit-Reset-Tokens"], []any{reset}) {
		t.Fatalf("missing first-attempt rate limits: %+v", headers)
	}
	if headers["X-Ratelimit-Api-Key"] != nil || first["cached_input_tokens"] != nil || first["uncached_input_tokens"] != nil || first["usage_known"] != false {
		t.Fatal("rejected attempt leaked a header or invented cache usage")
	}
	if len(second["rate_limit_headers"].(map[string]any)) != 1 || second["cached_input_tokens"] != float64(80) || second["uncached_input_tokens"] != float64(20) || second["total_tokens"] != float64(103) {
		t.Fatal("retry inherited headers or double-counted cached tokens")
	}
	for _, row := range []map[string]any{first, second} {
		if row["request_output_limits"].(map[string]any)["max_completion_tokens"] != float64(32768) {
			t.Fatal("request cap was lost on retry")
		}
		if _, err := time.Parse(time.RFC3339Nano, row["attempt_started_at"].(string)); err != nil {
			t.Fatal("missing precise upstream start time")
		}
	}
	for _, secret := range []string{"PRIVATE_PROMPT", "PRIVATE_HEADER", "upstream-secret-0"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("rate diagnostics leaked request content or credentials")
		}
	}
	// Read the encrypted archive again, as a restarted gateway would.
	reloaded := New(s.Config)
	if err := reloaded.LoadSessions(); err != nil {
		t.Fatal(err)
	}
	r, err := reloaded.Sessions.Record("rate-diagnostics", w.Header().Get("X-Request-ID"))
	if err != nil || len(r.Attempts) != 2 {
		t.Fatalf("attempts did not survive archive reload: %v", err)
	}
	a, b := r.Attempts[0], r.Attempts[1]
	if a.RateLimitHeaders["Retry-After"][0] != "120" || len(a.RateLimitHeaders["X-Ratelimit-Remaining-Requests"]) != 2 || a.RequestOutputLimits["max_completion_tokens"] != 32768 || len(b.RateLimitHeaders) != 1 || b.Usage.Cached != 80 || !slices.Contains(b.Usage.Reported, "cached_input_tokens") {
		t.Fatal("archive lost per-attempt telemetry")
	}
	if stamp, _ := time.Parse(time.RFC3339Nano, first["attempt_started_at"].(string)); !stamp.Equal(a.StartedAt) {
		t.Fatal("log and archive start timestamps differ")
	}
}

func TestRateLimitHeadersAreAllowlistedRedactedAndBounded(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), 1)
	h := http.Header{
		"retry-after":                        {"Wed, 21 Oct 2026 07:28:00 GMT"},
		"RateLimit":                          {`"default";r=5;t=10`},
		"RateLimit-Policy":                   {`"default";q=10;w=60`},
		"Anthropic-Ratelimit-Requests-Limit": {"50"},
		"X-Rate-Limit-Reset":                 {"1792300000"},
		"X-RateLimit-Remaining-Tokens":       {"upstream-secret-0 sk-extra-secret"},
		"Set-Cookie":                         {"PRIVATE_COOKIE"},
		"Authorization":                      {"PRIVATE_AUTH"},
		"X-RateLimit-Api-Key":                {"PRIVATE_KEY"},
		"X-RateLimit-Reset-Secret":           {"PRIVATE_SECRET"},
	}
	headers, truncated := observeRateLimitHeaders(h, s.logSanitizer())
	if truncated || len(headers) != 6 || headers["Retry-After"][0] != h["retry-after"][0] || headers["X-Ratelimit-Remaining-Tokens"][0] != "[REDACTED] [REDACTED]" {
		t.Fatalf("wrong safe rate headers: %+v", headers)
	}
	b, _ := json.Marshal(headers)
	if strings.Contains(string(b), "PRIVATE_") || h["X-RateLimit-Remaining-Tokens"][0] != "upstream-secret-0 sk-extra-secret" {
		t.Fatal("collector leaked secrets or mutated response headers")
	}
	for key := range h {
		if isRateLimitHeader(key) && !strings.EqualFold(key, "Retry-After") {
			h[key] = []string{strings.Repeat("x", 5000), strings.Repeat("y", 5000), strings.Repeat("z", 5000), strings.Repeat("a", 5000), "fifth-value"}
		}
	}
	headers, truncated = observeRateLimitHeaders(h, s.logSanitizer())
	if !truncated || headers["Retry-After"][0] != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Fatal("oversized diagnostics must be marked and preserve Retry-After")
	}
	budget := 0
	for key, values := range headers {
		if len(values) > 4 {
			t.Fatal("unbounded duplicate header values")
		}
		for _, value := range values {
			budget += len(key) + len(value)
		}
	}
	if budget > 4096 {
		t.Fatal("rate header observation exceeded its byte budget")
	}
}

func TestOutputLimitObservationDoesNotInventOrRoundCaps(t *testing.T) {
	for _, tc := range []struct {
		name, ct, body string
		want           map[string]int64
	}{
		{"multiple", "application/json; charset=utf-8", `{"max_tokens":4096,"max_completion_tokens":32768,"max_output_tokens":8192}`, map[string]int64{"max_tokens": 4096, "max_completion_tokens": 32768, "max_output_tokens": 8192}},
		{"large-integer", "application/json", `{"max_tokens":9007199254740993}`, map[string]int64{"max_tokens": 9007199254740993}},
		{"zero", "application/json", `{"max_tokens":0}`, map[string]int64{"max_tokens": 0}},
		{"native", "application/vnd.test+json", `{"generationConfig":{"maxOutputTokens":512},"generation_config":{"max_output_tokens":256}}`, map[string]int64{"generationConfig.maxOutputTokens": 512, "generation_config.max_output_tokens": 256}},
		{"unset", "application/json", `{"messages":[{"content":"PRIVATE_PROMPT"}]}`, nil},
		{"null", "application/json", `{"max_tokens": null,"max_completion_tokens":"PRIVATE_PROMPT","max_output_tokens":1.5}`, nil},
		{"invalid-integers", "application/json", `{"max_tokens":-1,"max_completion_tokens":9223372036854775808}`, nil},
		{"invalid-json", "application/json", `{"max_tokens":5`, nil},
		{"opaque", "application/octet-stream", `{"max_tokens":100}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			r.Header.Set("Content-Type", tc.ct)
			body := []byte(tc.body)
			if got := requestOutputLimits(r, body); !reflect.DeepEqual(got, tc.want) || !bytes.Equal(body, []byte(tc.body)) {
				t.Fatalf("output cap observation: got %v want %v", got, tc.want)
			}
		})
	}
}

func TestCacheLogDistinguishesUnreportedZeroAndSSEUsage(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		cached, uncached        any
	}{
		{"no-usage", "application/json", `{}`, nil, nil},
		{"cache-unreported", "application/json", `{"usage":{"input_tokens":10,"output_tokens":2}}`, nil, nil},
		{"cache-zero", "application/json", `{"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}}`, float64(0), float64(10)},
		{"cache-exceeds-input", "application/json", `{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":20}}}`, float64(20), nil},
		{"sse", "text/event-stream", "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":3,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\ndata: [DONE]\n\n", float64(80), float64(20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				io.WriteString(w, tc.body)
			}), 1)
			logs := captureLogs(s)
			w := call(s, "POST", "/v1/chat/completions", `{"model":"m","max_tokens":4096}`)
			if w.Code != 200 || w.Body.String() != tc.body {
				t.Fatal("cache observation changed response bytes")
			}
			row := logRecords(t, logs)[0]
			if row["cached_input_tokens"] != tc.cached || row["uncached_input_tokens"] != tc.uncached {
				t.Fatalf("wrong cache provenance: %+v", row)
			}
		})
	}
}

func TestTransportFailureDoesNotInheritEarlierRateLimitHeaders(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	}), 2)
	base := s.client.Transport
	s.client.Transport = streamTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "Bearer upstream-secret-1" {
			return nil, errors.New("test transport unavailable")
		}
		return base.RoundTrip(r)
	})
	logs := captureLogs(s)
	w := call(s, "POST", "/v1/chat/completions", `{"model":"m","max_output_tokens":1024}`)
	rows := logRecords(t, logs)
	if w.Code != 502 || len(rows) != 3 || rows[0]["rate_limit_headers"] == nil || rows[1]["rate_limit_headers"] != nil || rows[1]["cached_input_tokens"] != nil {
		t.Fatal("transport failure inherited telemetry from the rejected account")
	}
}
