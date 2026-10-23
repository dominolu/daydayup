package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"safuno/internal/config"
	"safuno/internal/redact"
)

func redactionTestConfig(t *testing.T, storeDir string) *redact.Config {
	t.Helper()
	os.Setenv("GATEWAY_REDACTION_STORE_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Cleanup(func() { os.Unsetenv("GATEWAY_REDACTION_STORE_KEY") })
	return &redact.Config{
		Enabled:     true,
		StoreFile:   filepath.Join(storeDir, "map.enc"),
		StoreKeyEnv: "GATEWAY_REDACTION_STORE_KEY",
	}
}

func redactedGateway(t *testing.T, upstream http.Handler) (*Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	c := config.Config{
		DefaultProvider:        "sensenova",
		GatewayKey:             "gateway-secret-at-least-16",
		AdminKey:               "admin-secret-at-least-16",
		RequestTimeoutSeconds:  5,
		RefreshIntervalSeconds: 60,
		MaxRequestBytes:        1 << 20,
		MaxAttempts:            3,
		DBFile:                 filepath.Join(t.TempDir(), "gateway.db"),
		Redaction:              redactionTestConfig(t, t.TempDir()),
	}
	c.Accounts = []config.Account{{ID: "a1", Provider: "sensenova", BaseURL: up.URL, APIKey: "upstream-secret", MaxInFlight: 8}}
	s := New(c)
	if err := s.RedactionError(); err != nil {
		t.Fatalf("redaction engine: %v", err)
	}
	s.logger = discardLogger()
	return s, up
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRedactionMasksRequestAndRestoresJSONResponse(t *testing.T) {
	var upstreamBody atomic.Value
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		// Echo the masked content back as if the model answered.
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		echo := ""
		if len(req.Messages) > 0 {
			echo = req.Messages[0].Content
		}
		resp := map[string]any{
			"id": "cmpl-test", "object": "chat.completion", "model": "m",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "回复：" + echo}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	s, _ := redactedGateway(t, upstream)

	body := `{"model":"m","messages":[{"role":"user","content":"我的电话 13800138000，邮箱 user@example.com"}]}`
	rec := call(s, "POST", "/v1/chat/completions", body)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	sent := upstreamBody.Load().(string)
	if strings.Contains(sent, "13800138000") || strings.Contains(sent, "user@example.com") {
		t.Fatalf("sensitive values leaked upstream: %s", sent)
	}
	if !strings.Contains(sent, "{{PHONE_") || !strings.Contains(sent, "{{EMAIL_") {
		t.Fatalf("upstream body missing placeholders: %s", sent)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "13800138000") || !strings.Contains(out, "user@example.com") {
		t.Fatalf("client response not restored: %s", out)
	}
	if strings.Contains(out, "{{PHONE_") || strings.Contains(out, "{{EMAIL_") {
		t.Fatalf("client response leaks placeholders: %s", out)
	}
}

func TestRedactionEscapedJSONReachesUpstreamAndRoundtrips(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model     string                           `json:"model"`
					Messages  []struct{ Role, Content string } `json:"messages"`
					MaxTokens int                              `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"message":"invalid arguments","type":"invalid_request_error","code":"3"}`)
					return
				}
				if req.Model != "m" || req.MaxTokens != 123 || len(req.Messages) != 1 || req.Messages[0].Role != "user" {
					t.Errorf("upstream request structure changed: %+v", req)
					w.WriteHeader(400)
					return
				}
				content := req.Messages[0].Content
				if !strings.HasPrefix(content, "你好\n") || !strings.Contains(content, "{{EMAIL_") || strings.Contains(content, "user@example.com") {
					t.Errorf("upstream content was not safely masked: %q", content)
				}
				w.Header().Set("Content-Type", "application/json")
				message := "message"
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					message = "delta"
					_, _ = io.WriteString(w, "data: ")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, message: map[string]any{"content": content}, "finish_reason": "stop"}}})
				if stream {
					_, _ = io.WriteString(w, "\ndata: [DONE]\n\n")
					_ = http.NewResponseController(w).Flush()
				}
			})
			s, _ := redactedGateway(t, upstream)
			body := `{"model":"m","messages":[{"role":"user","content":"\u4f60\u597d\nuser@example.com"}],"max_tokens":123,"stream":` + strconv.FormatBool(stream) + `}`
			rec := call(s, "POST", "/v1/chat/completions", body)
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "user@example.com") || strings.Contains(rec.Body.String(), "{{EMAIL_") {
				t.Fatalf("client content was not restored: %s", rec.Body.String())
			}
			if stream && !strings.Contains(rec.Body.String(), "data: [DONE]") {
				t.Fatalf("stream terminal event missing: %s", rec.Body.String())
			}
		})
	}
}

func TestRedactionRestoresStreamingResponse(t *testing.T) {
	var upstreamBody atomic.Value
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody.Store(string(body))
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		echo := ""
		if len(req.Messages) > 0 {
			echo = req.Messages[0].Content // contains the masked placeholder
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := http.NewResponseController(w)
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		_ = f.Flush()
		// The model echoes the masked content back; the gateway must restore it.
		_, _ = w.Write([]byte("data: {\"id\":\"2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"已记录：" + echo + "\"}}]}\n\n"))
		_ = f.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		_ = f.Flush()
	})
	s, _ := redactedGateway(t, upstream)

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"记录我的电话 13800138000"}]}`
	rec := call(s, "POST", "/v1/chat/completions", body)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	sent := upstreamBody.Load().(string)
	if strings.Contains(sent, "13800138000") {
		t.Fatalf("sensitive value leaked upstream: %s", sent)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "13800138000") {
		t.Fatalf("client stream not restored: %s", out)
	}
	if strings.Contains(out, "{{PHONE_") {
		t.Fatalf("client stream leaks placeholder: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("terminal event missing: %s", out)
	}
}

func TestRedactionDisabledByDefault(t *testing.T) {
	// Without a redaction config the gateway stays transparent.
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	up := httptest.NewServer(upstream)
	defer up.Close()
	c := config.Config{
		DefaultProvider:        "sensenova",
		GatewayKey:             "gateway-secret-at-least-16",
		AdminKey:               "admin-secret-at-least-16",
		RequestTimeoutSeconds:  5,
		RefreshIntervalSeconds: 60,
		MaxRequestBytes:        1 << 20,
		MaxAttempts:            3,
	}
	c.Accounts = []config.Account{{ID: "a1", Provider: "sensenova", BaseURL: up.URL, APIKey: "upstream-secret", MaxInFlight: 8}}
	s := New(c)
	if s.redact != nil {
		t.Fatal("redaction engine must not start when disabled")
	}
	body := `{"model":"m","messages":[{"role":"user","content":"电话 13800138000"}]}`
	rec := call(s, "POST", "/v1/chat/completions", body)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != body {
		t.Fatalf("transparent gateway changed body: %s", rec.Body.String())
	}
}

func TestRedactionMultipartNotMasked(t *testing.T) {
	var upstreamBody atomic.Value
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	s, _ := redactedGateway(t, upstream)
	// Multipart bodies must pass through byte-for-byte (binary safety).
	payload := "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--b\r\nContent-Disposition: form-data; name=\"image\"; filename=\"x.png\"\r\nContent-Type: image/png\r\n\r\n\x89PNG\r\n--b--\r\n"
	r := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader([]byte(payload)))
	r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if got := upstreamBody.Load().(string); got != payload {
		t.Fatalf("multipart body altered: %q != %q", got, payload)
	}
}

func TestRedactionAdminEndpoint(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	s, _ := redactedGateway(t, upstream)
	r := httptest.NewRequest("GET", "/admin/v1/redaction", nil)
	r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("admin redaction status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Enabled bool         `json:"enabled"`
		Stats   redact.Stats `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("admin response: %v", err)
	}
	if !resp.Enabled {
		t.Fatal("redaction admin reports disabled")
	}
}

// slogCapture records slog records so tests can assert structured log output.
type slogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *slogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *slogCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.records = append(c.records, r.Clone())
	c.mu.Unlock()
	return nil
}
func (c *slogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *slogCapture) WithGroup(string) slog.Handler      { return c }

// spans returns every "redaction span" record as an attribute map.
func (c *slogCapture) spans() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, r := range c.records {
		if r.Message != "redaction span" {
			continue
		}
		attrs := map[string]any{}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
		out = append(out, attrs)
	}
	return out
}

func TestRedactionSpanLogsMaskAndRestorePerRequest(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			return
		}
		echo := ""
		if len(req.Messages) > 0 {
			echo = req.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": echo}, "finish_reason": "stop"}}})
	})
	s, _ := redactedGateway(t, upstream)
	capture := &slogCapture{}
	s.logger = slog.New(capture)

	body := `{"model":"m","messages":[{"role":"user","content":"电话 13800138000，邮箱 user@example.com"}]}`
	rec := call(s, "POST", "/v1/chat/completions", body)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	requestID := rec.Header().Get("X-Request-ID")

	var masked, restored []map[string]any
	for _, span := range capture.spans() {
		if span["request_id"] != requestID {
			t.Fatalf("span missing request correlation: %v", span)
		}
		switch span["kind"] {
		case "masked":
			masked = append(masked, span)
		case "restored":
			restored = append(restored, span)
		default:
			t.Fatalf("unexpected span kind: %v", span)
		}
	}
	if len(masked) != 2 || len(restored) != 2 {
		t.Fatalf("expected 2 masked and 2 restored spans, got %d/%d", len(masked), len(restored))
	}
	// Every masked span names a rule and pairs a distinct original with its
	// placeholder; the response restore must reverse exactly that mapping.
	originalByPlaceholder := map[string]string{}
	labelByPlaceholder := map[string]string{}
	for _, span := range masked {
		label, _ := span["label"].(string)
		original, ok := span["original"].(string)
		placeholder, ok2 := span["placeholder"].(string)
		if label != "PHONE" && label != "EMAIL" {
			t.Fatalf("masked span label: %v", span)
		}
		if !ok || !ok2 || original == "" || placeholder == "" || original == placeholder {
			t.Fatalf("masked span mapping: %v", span)
		}
		if !strings.HasPrefix(placeholder, "{{"+label+"_") {
			t.Fatalf("masked span placeholder mismatches its label: %v", span)
		}
		originalByPlaceholder[placeholder] = original
		labelByPlaceholder[placeholder] = label
	}
	for _, span := range restored {
		label, _ := span["label"].(string)
		original, ok := span["original"].(string)
		placeholder, ok2 := span["placeholder"].(string)
		if !ok || !ok2 {
			t.Fatalf("restored span mapping: %v", span)
		}
		want := originalByPlaceholder[placeholder]
		if want == "" || original != want {
			t.Fatalf("restored mapping differs from the masked one: %v vs %q", span, want)
		}
		if label != labelByPlaceholder[placeholder] {
			t.Fatalf("restored label differs from the masked one: %v", span)
		}
	}
}

func TestRedactionRulesHotReload(t *testing.T) {
	var upstreamBody atomic.Value
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	s, _ := redactedGateway(t, upstream)

	// Initially no custom words: "alpha-project" passes through unmasked.
	_ = call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"代号 alpha-project"}]}`)
	if got := upstreamBody.Load().(string); strings.Contains(got, "{{ALPHA_") {
		t.Fatalf("unexpected pre-reload masking: %s", got)
	}
	// Hot-reload adds the custom word.
	body := `{"custom_words":{"ALPHA":["alpha-project"]}}`
	r := httptest.NewRequest("PUT", "/admin/v1/redaction/rules", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("rules reload status %d: %s", w.Code, w.Body.String())
	}
	_ = call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"代号 alpha-project"}]}`)
	got := upstreamBody.Load().(string)
	if !strings.Contains(got, "{{ALPHA_") {
		t.Fatalf("custom word not masked after reload (upstream saw): %s", got)
	}
	if strings.Contains(got, "alpha-project") {
		t.Fatalf("custom word leaked to upstream after reload: %s", got)
	}
}

func TestRedactionToolGuardObserveInStream(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := http.NewResponseController(w)
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"type\":\"response.output_item.delta\",\"delta\":{\"partial_json\":\"{\\\"cmd\\\":\\\"rm -rf /\\\"}\"}}\n\n"))
		_ = f.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		_ = f.Flush()
	})
	s, _ := redactedGateway(t, upstream)
	// Enable tool guard observe.
	body := `{"tool_guard":{"enabled":true,"mode":"observe"}}`
	r := httptest.NewRequest("PUT", "/admin/v1/redaction/rules", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("rules reload status %d: %s", w.Code, w.Body.String())
	}
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"执行命令"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// Observe mode: the tool call still reaches the client, and the finding is
	// recorded in the redaction stats.
	r2 := httptest.NewRequest("GET", "/admin/v1/redaction", nil)
	r2.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
	w2 := httptest.NewRecorder()
	s.ServeHTTP(w2, r2)
	if w2.Code != 200 {
		t.Fatalf("admin status %d", w2.Code)
	}
	var resp struct {
		Stats redact.Stats `json:"stats"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("admin json: %v", err)
	}
	if resp.Stats.ToolFindings != 1 {
		t.Fatalf("expected 1 tool finding, got %d", resp.Stats.ToolFindings)
	}
}
