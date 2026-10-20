package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"daydayup/internal/config"
	"daydayup/internal/redact"
)

var placeholderRegex = regexp.MustCompile(`{{\w+_[a-z]{6}}}`)

const chatSSEStream = `data: {"id":"chatcmpl-1","model":"m1","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}

data: {"id":"chatcmpl-1","model":"m1","choices":[{"index":0,"delta":{"content":"世界"}}]}

data: {"id":"chatcmpl-1","model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search","arguments":"{\"q\":"}}]}}]}

data: {"id":"chatcmpl-1","model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"测试\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-1","model":"m1","usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21}}

data: [DONE]

`

// chatSSEHandler feeds a canned Chat SSE stream to its clients.
func chatSSEHandler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m1"}}})
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path: %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("upstream body not json: %v", err)
		}
		if body["model"] != "m1" {
			t.Errorf("upstream model: %v", body["model"])
		}
		stream, _ := body["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"非流式回复"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatSSEStream))
	})
}

func bridgeGateway(t *testing.T, upstream http.Handler, bridges config.BridgesConfig) (*Server, *httptest.Server) {
	t.Helper()
	gw := httptest.NewServer(upstream)
	t.Cleanup(gw.Close)
	c := config.Config{DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 20, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Bridges: bridges}
	c.Accounts = append(c.Accounts, config.Account{ID: "a1", Provider: "sensenova", BaseURL: gw.URL, APIKey: "upstream-secret-1", MaxInFlight: 8})
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return s, gw
}

func TestMessagesToChatConversion(t *testing.T) {
	body := `{
		"model":"claude-sonnet","max_tokens":100,"stream":true,
		"system":"你是助手",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"你好"}]},
			{"role":"assistant","content":[{"type":"text","text":"hi"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"结果"}]}]}
		],
		"tools":[{"name":"search","description":"搜","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]
	}`
	converted, err := messagesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(converted, &chat); err != nil {
		t.Fatal(err)
	}
	if chat["model"] != "claude-sonnet" {
		t.Fatalf("model: %v", chat["model"])
	}
	if chat["stream_options"] == nil {
		t.Fatalf("stream_options missing")
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("expect 4 messages (system,user,assistant,tool), got %d", len(msgs))
	}
	last := msgs[3].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "tu1" || !strings.Contains(last["content"].(string), "结果") {
		t.Fatalf("tool result not mapped to role:tool: %v", last)
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools: %v", chat["tools"])
	}
}

func TestResponsesToChatConversion(t *testing.T) {
	body := `{
		"model":"gpt-5.5","max_output_tokens":200,"stream":true,
		"instructions":"遵循规则",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]},
			{"type":"function_call","call_id":"fc1","name":"search","arguments":"{\"q\":\"x\"}"},
			{"type":"function_call_output","call_id":"fc1","output":"结果"}
		],
		"tools":[{"type":"function","name":"search","function":{"description":"搜","parameters":{"type":"object"}}}]
	}`
	converted, err := responsesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(converted, &chat); err != nil {
		t.Fatal(err)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 4 { // system + user + assistant(tool_call) + tool
		t.Fatalf("got %d messages", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("instructions not mapped to system")
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "assistant" {
		t.Fatalf("function_call not mapped: %v", toolMsg)
	}
	tc := toolMsg["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "fc1" || tc["type"] != "function" {
		t.Fatalf("tool_calls wrong: %v", tc)
	}
	if msgs[3].(map[string]any)["role"] != "tool" {
		t.Fatalf("function_call_output not mapped")
	}
}

func TestBridgeMessagesStreamingE2E(t *testing.T) {
	s, _ := bridgeGateway(t, chatSSEHandler(t), config.BridgesConfig{Messages: true})
	body := `{"model":"m1","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"你好"}]}`
	rec := call(s, "POST", "/v1/messages", body)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		`event: message_start`,
		`"type":"message"`,
		`event: content_block_start`,
		`"type":"text"`,
		`event: content_block_delta`,
		`"text_delta"`,
		"你好",
		`"type":"tool_use"`,
		`"id":"call_1"`,
		`"input_json_delta"`,
		`event: message_delta`,
		`"stop_reason":"tool_use"`,
		`"input_tokens":12,"output_tokens":9`,
		`event: message_stop`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestBridgeMessagesNonStreamingE2E(t *testing.T) {
	s, _ := bridgeGateway(t, chatSSEHandler(t), config.BridgesConfig{Messages: true})
	body := `{"model":"m1","max_tokens":50,"stream":false,"messages":[{"role":"user","content":"你好"}]}`
	rec := call(s, "POST", "/v1/messages", body)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var msg struct {
		Type        string `json:"type"`
		Content     []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("invalid Messages JSON: %v\n%s", err, rec.Body.String())
	}
	if msg.Type != "message" || msg.StopReason != "end_turn" {
		t.Fatalf("shape: %+v", msg)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "非流式回复" {
		t.Fatalf("content: %+v", msg.Content)
	}
	if msg.Usage.Input != 5 || msg.Usage.Output != 4 {
		t.Fatalf("usage: %+v", msg.Usage)
	}
}

func TestBridgeResponsesStreamingE2E(t *testing.T) {
	s, _ := bridgeGateway(t, chatSSEHandler(t), config.BridgesConfig{Responses: true})
	body := `{"model":"m1","stream":true,"input":"你好"}`
	rec := call(s, "POST", "/v1/responses", body)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		`event: response.created`,
		`"object":"response"`,
		`event: response.output_item.added`,
		`event: response.output_text.delta`,
		`"delta":"你好"`,
		`event: response.output_text.done`,
		`"type":"function_call"`,
		`event: response.function_call_arguments.delta`,
		`event: response.completed`,
		`"status":"completed"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestBridgeResponsesNonStreamingE2E(t *testing.T) {
	s, _ := bridgeGateway(t, chatSSEHandler(t), config.BridgesConfig{Responses: true})
	body := `{"model":"m1","stream":false,"input":"你好"}`
	rec := call(s, "POST", "/v1/responses", body)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type string `json:"type"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid Responses JSON: %v\n%s", err, rec.Body.String())
	}
	if resp.Object != "response" || resp.Status != "completed" || len(resp.Output) != 1 {
		t.Fatalf("shape: %+v", resp)
	}
}

func TestBridgeDisabledPassthrough(t *testing.T) {
	seenPath := ""
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	s, _ := bridgeGateway(t, up, config.BridgesConfig{})
	rec := call(s, "POST", "/v1/messages", `{"model":"m1","messages":[],"max_tokens":1}`)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if seenPath != "/v1/messages" {
		t.Fatalf("expected raw passthrough to /v1/messages, upstream saw %s", seenPath)
	}
}

func TestBridgeConversionError(t *testing.T) {
	s, _ := bridgeGateway(t, chatSSEHandler(t), config.BridgesConfig{Messages: true})
	rec := call(s, "POST", "/v1/messages", `{"stream":true,"messages":[]}`) // no model
	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBridgeAnthropicEnd(t *testing.T) {
	// A stream with finish_reason but no usage chunk must still emit
	// message_stop through End().
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m1"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	s, _ := bridgeGateway(t, up, config.BridgesConfig{Messages: true})
	rec := call(s, "POST", "/v1/messages", `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if !strings.Contains(out, `event: message_stop`) {
		t.Fatalf("missing message_stop in:\n%s", out)
	}
}
func TestMessagesToChatToolCalls(t *testing.T) {
	// Full multi-turn tool flow: user -> assistant(tool_use) -> user(tool_result).
	body := `{
		"model":"claude-sonnet","max_tokens":50,
		"messages":[
			{"role":"user","content":"搜索天气"},
			{"role":"assistant","content":[
				{"type":"text","text":"让我查一下"},
				{"type":"tool_use","id":"toolu_01","name":"search","input":{"q":"weather"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"晴 22°C"}]}
			]}
		],
		"tools":[{"name":"search","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]
	}`
	converted, err := messagesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(converted, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("expect 3 messages, got %d: %s", len(chat.Messages), converted)
	}
	// assistant message carries tool_calls with the original id.
	assistant := chat.Messages[1]
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls missing: %s", converted)
	}
	fn := calls[0].(map[string]any)
	if fn["id"] != "toolu_01" || fn["type"] != "function" {
		t.Fatalf("tool_call wrong: %v", fn)
	}
	fnBody := fn["function"].(map[string]any)
	if fnBody["name"] != "search" {
		t.Fatalf("tool name: %v", fnBody)
	}
	// tool_result maps to a role:tool message carrying tool_call_id.
	toolMsg := chat.Messages[2]
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_01" {
		t.Fatalf("tool message wrong: %v", toolMsg)
	}
	if !strings.Contains(toolMsg["content"].(string), "晴 22°C") {
		t.Fatalf("tool content: %v", toolMsg["content"])
	}
}

func TestMessagesToChatPlainString(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"你好"}]}`
	converted, err := messagesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(converted, &chat); err != nil {
		t.Fatal(err)
	}
	if chat.Messages[0]["content"] != "你好" {
		t.Fatalf("content: %v", chat.Messages[0]["content"])
	}
}

func TestBridgeMessagesStreamIgnoredByUpstream(t *testing.T) {
	// Client asks stream:true, upstream answers plain JSON: the bridge must
	// pass the body through raw instead of hanging the client.
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m1"}}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"raw"},"finish_reason":"stop"}]}`))
	})
	s, _ := bridgeGateway(t, up, config.BridgesConfig{Messages: true})
	rec := call(s, "POST", "/v1/messages", `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	// Raw chat JSON must reach the client (visible failure), not silence.
	if rec.Code != 200 || !strings.Contains(out, "chat.completion") {
		t.Fatalf("expected raw passthrough, got %d: %s", rec.Code, out)
	}
}

func TestBridgeMessagesNonStreamFallbackRaw(t *testing.T) {
	// Client asks non-stream, upstream returns SSE anyway: finalize must fall
	// back to raw bytes instead of an empty 200.
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m1"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"sse\"}}]}\n\ndata: [DONE]\n\n"))
	})
	s, _ := bridgeGateway(t, up, config.BridgesConfig{Messages: true})
	rec := call(s, "POST", "/v1/messages", `{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("expected raw SSE fallback, got: %s", out)
	}
}

func TestAnthropicToolNameArrivesLate(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m1"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// First chunk: id only, arguments start; name arrives later.
		arg1 := `{"q":`
		arg2 := `"测试"}`
		fmt.Fprintf(w, "data: {\"id\":\"x\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_9\",\"function\":{\"arguments\":%s}}]}}]}\n\n", jsonString(arg1))
		fmt.Fprintf(w, "data: {\"id\":\"x\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"search\",\"arguments\":%s}}]}}]}\n\n", jsonString(arg2))
		fmt.Fprint(w, "data: {\"id\":\"x\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"model\":\"m1\",\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	s, _ := bridgeGateway(t, up, config.BridgesConfig{Messages: true})
	rec := call(s, "POST", "/v1/messages", `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if !strings.Contains(out, `"name":"search"`) {
		t.Fatalf("tool name missing from start block:\n%s", out)
	}
	if !strings.Contains(out, `"id":"call_9"`) {
		t.Fatalf("tool id missing:\n%s", out)
	}
	if !strings.Contains(out, `{\"q\":\"测试\"}`) {
		t.Fatalf("buffered arguments not flushed:\n%s", out)
	}
}

func TestBridgeMessagesWithRedaction(t *testing.T) {
	// Redaction must survive the bridge: the upstream sees the masked body in
	// Chat form, and the client receives the restored text in Messages form.
	var upstreamBody string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m1"}}})
			return
		}
		b, _ := io.ReadAll(r.Body)
		upstreamBody = string(b)
		// Echo back whatever placeholder the masked request carried.
		placeholder := "{{PLACEHOLDER_unknown}}"
		if m := placeholderRegex.Find(b); len(m) > 0 {
			placeholder = string(m)
		}
		reply := `{"id":"c1","model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"联系 ` + placeholder + ` 即可"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	})
	dir := t.TempDir()
	// Build a gateway with redaction enabled through the config struct.
	gw := httptest.NewServer(up)
	t.Cleanup(gw.Close)
	c := config.Config{
		DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16",
		RequestTimeoutSeconds: 20, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3,
		Bridges: config.BridgesConfig{Messages: true},
		Redaction: &redact.Config{Enabled: true, StoreFile: filepath.Join(dir, "map.enc"), StoreKeyEnv: "GATEWAY_REDACTION_STORE_KEY", Scope: "default", MaxMappings: 10000, TokenTTLSeconds: 3600},
	}
	c.Accounts = append(c.Accounts, config.Account{ID: "a1", Provider: "sensenova", BaseURL: gw.URL, APIKey: "upstream-secret-1", MaxInFlight: 8})
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := s.LoadSessions(); err != nil {
		t.Fatal(err)
	}
	rec := call(s, "POST", "/v1/messages", `{"model":"m1","stream":false,"messages":[{"role":"user","content":"联系 alice@example.com 吧"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// The upstream must NOT see the plain email (masked then converted to Chat).
	if strings.Contains(upstreamBody, "alice@example.com") {
		t.Fatalf("upstream saw plaintext email: %s", upstreamBody)
	}
	if !strings.Contains(upstreamBody, `"messages"`) {
		t.Fatalf("upstream did not receive Chat form: %s", upstreamBody)
	}
	// The client must receive the restored email in Messages form.
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"message"`) {
		t.Fatalf("not a Messages response: %s", out)
	}
	if !strings.Contains(out, "alice@example.com") {
		t.Fatalf("email not restored for client: %s", out)
	}
}

func TestBridgeSingleEnabledOtherPathPassthrough(t *testing.T) {
	// Only responses bridge on; /v1/messages must pass through raw (upstream
	// decides), NOT return an empty 200.
	seen := ""
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"type":"not_found"}}`))
	})
	gw := httptest.NewServer(up)
	t.Cleanup(gw.Close)
	c := config.Config{DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Bridges: config.BridgesConfig{Responses: true}}
	c.Accounts = append(c.Accounts, config.Account{ID: "a1", Provider: "sensenova", BaseURL: gw.URL, APIKey: "upstream-secret-1", MaxInFlight: 8})
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := call(s, "POST", "/v1/messages", `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if seen != "/v1/messages" {
		t.Fatalf("upstream saw %s, want passthrough", seen)
	}
	if rec.Code != 200 {
		t.Fatalf("status %d (upstream replied 200 with error body)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"not_found"`) {
		t.Fatalf("upstream body lost: %s", rec.Body.String())
	}
}

func TestBridgeNonStreamUpstreamError(t *testing.T) {
	// Upstream 400 on a non-streaming bridged request: the client must get a
	// 400 (not 200) with the error body exactly once.
	var count int
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m1"}}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad"}}`))
		count++
	})
	s, _ := bridgeGateway(t, up, config.BridgesConfig{Messages: true})
	rec := call(s, "POST", "/v1/messages", `{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"invalid_request_error"`) {
		t.Fatalf("error body missing: %s", body)
	}
	if strings.Count(body, "invalid_request_error") != 1 {
		t.Fatalf("error body duplicated: %s", body)
	}
}
