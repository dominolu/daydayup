package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daydayup/internal/config"
)

func middlewareGateway(t *testing.T, upstream http.Handler, mw config.MiddlewareConfig) (*Server, *httptest.Server) {
	t.Helper()
	gw := httptest.NewServer(upstream)
	t.Cleanup(gw.Close)
	c := config.Config{DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 20, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Middleware: mw}
	c.Accounts = append(c.Accounts, config.Account{ID: "a1", Provider: "sensenova", BaseURL: gw.URL, APIKey: "upstream-secret-1", MaxInFlight: 8})
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return s, gw
}

func TestMiddlewareModelMapRequestAndResponse(t *testing.T) {
	var upstreamModel string
	var respModel string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("body: %v", err)
		}
		upstreamModel = body.Model
		respModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		reply, _ := json.Marshal(map[string]any{
			"id": "c1", "model": respModel, "object": "chat.completion",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3},
		})
		_, _ = w.Write(reply)
	})
	s, _ := middlewareGateway(t, up, config.MiddlewareConfig{ModelMap: map[string]string{"fast": "deepseek-v4.1-flash"}})
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if upstreamModel != "deepseek-v4.1-flash" {
		t.Fatalf("upstream model = %s, want renamed", upstreamModel)
	}
	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Model != "fast" {
		t.Fatalf("client model = %s, want alias", resp.Model)
	}
}

func TestMiddlewareSystemPromptInjection(t *testing.T) {
	var gotMessages []map[string]any
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(b, &body)
		gotMessages = body.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	mw := config.MiddlewareConfig{SystemPrompt: []config.SystemPromptRule{{MatchModel: "m", Text: "你是内部助手"}}}
	s, _ := middlewareGateway(t, up, mw)
	call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if len(gotMessages) != 2 || gotMessages[0]["role"] != "system" || gotMessages[0]["content"] != "你是内部助手" {
		t.Fatalf("system prompt not injected: %v", gotMessages)
	}
	// Idempotence: second call with existing identical prompt must not duplicate.
	call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"system","content":"你是内部助手"},{"role":"user","content":"hi"}]}`)
	if len(gotMessages) != 2 {
		t.Fatalf("duplicate system prompt injected: %v", gotMessages)
	}
}

func TestMiddlewareAgentMatchSkipsOthers(t *testing.T) {
	var gotMessages []map[string]any
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(b, &body)
		gotMessages = body.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	mw := config.MiddlewareConfig{SystemPrompt: []config.SystemPromptRule{{MatchAgent: "claude-code", Text: "只给 Claude Code"}}}
	s, _ := middlewareGateway(t, up, mw)
	// dsh session identity: rule must not apply.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DeepSeek-Harness-Session-Id", "sess-1")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if len(gotMessages) != 1 {
		t.Fatalf("rule applied to non-matched agent: %v", gotMessages)
	}
}

func TestMiddlewareThinkTagsStripStreaming(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"思考\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"回答\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	mw := config.MiddlewareConfig{ThinkTags: &config.ThinkTagsConfig{Strip: true}}
	s, _ := middlewareGateway(t, up, mw)
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if strings.Contains(out, "思考") {
		t.Fatalf("reasoning_content leaked: %s", out)
	}
	if !strings.Contains(out, "回答") {
		t.Fatalf("content lost: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("[DONE] lost: %s", out)
	}
}

func TestMiddlewareModelMapThroughBridge(t *testing.T) {
	var upstreamModel string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "deepseek-v4.1-flash"}}})
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &body)
		upstreamModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	gw := httptest.NewServer(up)
	t.Cleanup(gw.Close)
	mw := config.MiddlewareConfig{ModelMap: map[string]string{"fast": "deepseek-v4.1-flash"}}
	c := config.Config{DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 20, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Middleware: mw, Bridges: config.BridgesConfig{Messages: true}}
	c.Accounts = append(c.Accounts, config.Account{ID: "a1", Provider: "sensenova", BaseURL: gw.URL, APIKey: "upstream-secret-1", MaxInFlight: 8})
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := call(s, "POST", "/v1/messages", `{"model":"fast","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if upstreamModel != "deepseek-v4.1-flash" {
		t.Fatalf("bridge upstream model = %s", upstreamModel)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"message"`) || !strings.Contains(out, "ok") {
		t.Fatalf("bridge response broken: %s", out)
	}
}
func TestMiddlewareThinkTagsKeepsErrorEvents(t *testing.T) {
	// A mid-stream error event must survive the rewriter untouched.
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"部分\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"error\":{\"type\":\"upstream_error\",\"message\":\"坏了\"}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	mw := config.MiddlewareConfig{ThinkTags: &config.ThinkTagsConfig{Strip: true}}
	s, _ := middlewareGateway(t, up, mw)
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if !strings.Contains(out, "upstream_error") || !strings.Contains(out, "坏了") {
		t.Fatalf("error event dropped: %s", out)
	}
	if !strings.Contains(out, "部分") {
		t.Fatalf("content lost: %s", out)
	}
}

func TestMiddlewareUsageBundledWithChoices(t *testing.T) {
	// Some providers send the final text + finish_reason and the usage in ONE
	// event; the rewriter must keep both (no truncated tail, no lost terminal).
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"结尾\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	mw := config.MiddlewareConfig{ModelMap: map[string]string{"client-m": "m"}}
	s, _ := middlewareGateway(t, up, mw)
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"client-m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if !strings.Contains(out, "结尾") {
		t.Fatalf("final text delta dropped: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason dropped: %s", out)
	}
	if !strings.Contains(out, `"total_tokens":5`) {
		t.Fatalf("usage dropped: %s", out)
	}
	if !strings.Contains(out, `"model":"client-m"`) {
		t.Fatalf("model not renamed: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("[DONE] lost: %s", out)
	}
}

func TestMiddlewareBridgeCascadeRaw(t *testing.T) {
	// middleware + bridge, streaming request, upstream ignores stream:true and
	// returns JSON: the raw switcher must cascade through both layers so the
	// client receives the upstream body instead of hanging.
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m"}}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"raw-json"},"finish_reason":"stop"}]}`))
	})
	gw := httptest.NewServer(up)
	t.Cleanup(gw.Close)
	c := config.Config{DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 20, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3,
		Bridges: config.BridgesConfig{Messages: true},
		Middleware: config.MiddlewareConfig{ModelMap: map[string]string{"m": "m"}},
	}
	c.Accounts = append(c.Accounts, config.Account{ID: "a1", Provider: "sensenova", BaseURL: gw.URL, APIKey: "upstream-secret-1", MaxInFlight: 8})
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := call(s, "POST", "/v1/messages", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if !strings.Contains(out, "raw-json") {
		t.Fatalf("cascaded raw passthrough failed (client hung/empty): %s", out)
	}
}

func TestMiddlewareNonStreamKeepsBigIntegers(t *testing.T) {
	// Token counts beyond 2^53 must survive the non-streaming rewrite exactly
	// (no float64 coercion).
	big := "9007199254740993"
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok","reasoning_content":"隐藏"},"finish_reason":"stop"}],"usage":{"prompt_tokens":` + big + `,"completion_tokens":2,"total_tokens":` + big + `}}`))
	})
	mw := config.MiddlewareConfig{ThinkTags: &config.ThinkTagsConfig{Strip: true}}
	s, _ := middlewareGateway(t, up, mw)
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if strings.Contains(out, "隐藏") {
		t.Fatalf("reasoning_content leaked: %s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":`+big) || !strings.Contains(out, `"total_tokens":`+big) {
		t.Fatalf("big integer corrupted: %s", out)
	}
}
