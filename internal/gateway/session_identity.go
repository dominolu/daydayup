package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"safuno/internal/observe"
	"safuno/internal/sessions"
)

type sessionHeader struct{ Name, Namespace, Client string }

// These are conversation identifiers, not request IDs, user IDs or cache keys.
// Keep aliases together so header audit, validation and resolution cannot drift.
var nativeSessionHeaders = []sessionHeader{
	{"X-Claude-Code-Session-Id", "claude-code", "Claude Code"},
	{"X-OpenCode-Session", "opencode", "OpenCode"},
	{"X-DeepSeek-Harness-Session-Id", "dsh", "DSH"},
	// Newer Codex uses session-id for cache affinity; thread-id is the actual
	// conversation and must win when the two legitimately differ.
	{"Thread-Id", "compat", "Codex / compatible"},
	{"Thread_id", "compat", "Codex / compatible"},
	{"Session-Id", "compat", "Codex / compatible"},
	{"Session_id", "compat", "Codex / compatible"},
	{"X-Session-Affinity", "compat", "Pi / compatible"},
}

func clientHint(r *http.Request) string {
	ua := strings.ToLower(r.UserAgent())
	for _, c := range []struct{ Match, Name string }{
		{"deepseek-harness", "DSH"}, {"opencode", "OpenCode"}, {"claude-code", "Claude Code"}, {"claude-cli", "Claude Code"}, {"codex", "Codex"},
	} {
		if strings.Contains(ua, c.Match) {
			return c.Name
		}
	}
	return "Unknown"
}

func nativeSessionID(namespace, value string) string {
	id := namespace + ":" + value
	if sessions.ValidID(id) && !strings.HasPrefix(value, "sha256:") {
		return id
	}
	h := sha256.Sum256([]byte(value))
	return namespace + ":sha256:" + hex.EncodeToString(h[:])
}

func turnStateHash(value string) string {
	if value == "" {
		return ""
	}
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func validNativeID(value string) bool {
	if value == "" || len(value) > 512 || strings.TrimSpace(value) != value || strings.Contains(value, ",") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func resolveIdentity(r *http.Request, body []byte) (string, sessions.Identity, error) {
	identity := sessions.Identity{Client: clientHint(r)}
	// X-Session-ID is the portable gateway contract and remains authoritative.
	if values := r.Header.Values("X-Session-ID"); len(values) > 0 {
		if len(values) != 1 || !sessions.ValidID(values[0]) {
			return "", identity, errors.New("provide one valid X-Session-ID (1–128 ASCII letters, digits, dots, colons, underscores or hyphens)")
		}
		identity.Source, identity.ClientSessionID = "header:x-session-id", values[0]
		return values[0], identity, nil
	}
	var id string
	for _, header := range nativeSessionHeaders {
		values := r.Header.Values(header.Name)
		if len(values) == 0 {
			continue
		}
		if len(values) != 1 || !validNativeID(values[0]) {
			return "", identity, errors.New("provide a single nonempty native session header of at most 512 bytes without control characters or commas")
		}
		next := nativeSessionID(header.Namespace, values[0])
		threadAffinity := (identity.Source == "header:thread-id" || identity.Source == "header:thread_id") && (header.Name == "Session-Id" || header.Name == "Session_id" || header.Name == "X-Session-Affinity")
		if id != "" && identity.ClientSessionID != values[0] && !threadAffinity {
			return "", identity, errors.New("conflicting native session headers; use X-Session-ID to select the intended conversation")
		}
		if id == "" {
			id = next
			identity.Source, identity.ClientSessionID = "header:"+strings.ToLower(header.Name), values[0]
			if identity.Client == "Unknown" {
				identity.Client = header.Client
			}
		}
	}
	if id != "" {
		return id, identity, nil
	}
	var payload struct {
		Metadata struct {
			SessionID string `json:"session_id"`
			UserID    string `json:"user_id"`
		} `json:"metadata"`
		DSHSessionLog struct {
			Session struct {
				ID string `json:"id"`
			} `json:"session"`
		} `json:"dsh_session_log"`
	}
	_ = json.Unmarshal(body, &payload)
	if id = payload.Metadata.SessionID; id != "" {
		if !sessions.ValidID(id) {
			return "", identity, errors.New("metadata.session_id must be a valid gateway session ID")
		}
		identity.Source, identity.ClientSessionID = "body:metadata.session_id", id
		return id, identity, nil
	}
	if id = payload.DSHSessionLog.Session.ID; id != "" {
		if !validNativeID(id) {
			return "", identity, errors.New("invalid dsh_session_log.session.id")
		}
		identity.Source, identity.ClientSessionID = "body:dsh_session_log.session.id", id
		if identity.Client == "Unknown" {
			identity.Client = "DSH"
		}
		return nativeSessionID("dsh", id), identity, nil
	}
	// Some Anthropic-compatible adapters encode session_id inside user_id JSON.
	// A plain user/device/account identifier must never become a conversation.
	var user struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(payload.Metadata.UserID), &user) == nil && user.SessionID != "" {
		if !validNativeID(user.SessionID) {
			return "", identity, errors.New("invalid structured metadata.user_id session_id")
		}
		identity.Source, identity.ClientSessionID = "body:metadata.user_id.session_id", user.SessionID
		return nativeSessionID("compat", user.SessionID), identity, nil
	}
	return "", identity, nil
}

func historyHash(path string, history []json.RawMessage, context json.RawMessage) string {
	if len(history) == 0 {
		return ""
	}
	// Marshal a decoded tree to ignore object key order and insignificant space,
	// while preserving array order and JSON numbers exactly.
	b, _ := json.Marshal(struct {
		History []json.RawMessage
		Context json.RawMessage
	}{history, context})
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var tree any
	if dec.Decode(&tree) != nil {
		return ""
	}
	b, err := json.Marshal(tree)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(append([]byte(path+"\x00"), b...))
	return hex.EncodeToString(h[:])
}

// requestHistory returns a fingerprint of this input, plus evidence for an
// immediate tool-result continuation. It never matches an arbitrary prefix.
func requestHistory(path string, body []byte) (history, previous string, toolIDs []string) {
	var p struct {
		Messages     []json.RawMessage `json:"messages"`
		Input        []json.RawMessage `json:"input"`
		System       json.RawMessage   `json:"system"`
		Instructions json.RawMessage   `json:"instructions"`
	}
	if json.Unmarshal(body, &p) != nil {
		return
	}
	rows, context := p.Messages, p.System
	if path == "/v1/responses" {
		rows, context = p.Input, p.Instructions
	}
	if path != "/v1/chat/completions" && path != "/v1/messages" && path != "/v1/responses" {
		return
	}
	history = historyHash(path, rows, context)
	if len(rows) < 2 {
		return
	}
	type block struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		ID        string `json:"id"`
	}
	type message struct {
		Role       string `json:"role"`
		Type       string `json:"type"`
		ToolCallID string `json:"tool_call_id"`
		CallID     string `json:"call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
		Content json.RawMessage `json:"content"`
	}
	parsed := make([]message, len(rows))
	for i := range rows {
		if json.Unmarshal(rows[i], &parsed[i]) != nil {
			return history, "", nil
		}
	}
	i := len(rows) - 1
	var expected []string
	switch path {
	case "/v1/chat/completions":
		for i >= 0 && parsed[i].Role == "tool" {
			if len(toolIDs) >= 128 || len(parsed[i].ToolCallID) > 256 {
				return history, "", nil
			}
			toolIDs = observe.AddToolID(toolIDs, parsed[i].ToolCallID)
			if parsed[i].ToolCallID == "" {
				return history, "", nil
			}
			i--
		}
		if i < 1 || parsed[i].Role != "assistant" || len(toolIDs) == 0 {
			return history, "", nil
		}
		for _, call := range parsed[i].ToolCalls {
			expected = observe.AddToolID(expected, call.ID)
		}
	case "/v1/messages":
		if parsed[i].Role != "user" {
			return history, "", nil
		}
		var results []block
		if json.Unmarshal(parsed[i].Content, &results) != nil || len(results) == 0 {
			return history, "", nil
		}
		for _, b := range results {
			if len(toolIDs) >= 128 || len(b.ToolUseID) > 256 {
				return history, "", nil
			}
			if b.Type != "tool_result" || b.ToolUseID == "" {
				return history, "", nil
			}
			toolIDs = observe.AddToolID(toolIDs, b.ToolUseID)
		}
		i--
		if i < 1 || parsed[i].Role != "assistant" {
			return history, "", nil
		}
		var calls []block
		_ = json.Unmarshal(parsed[i].Content, &calls)
		for _, b := range calls {
			if b.Type == "tool_use" {
				expected = observe.AddToolID(expected, b.ID)
			}
		}
	case "/v1/responses":
		for i >= 0 && parsed[i].Type == "function_call_output" {
			if len(toolIDs) >= 128 || len(parsed[i].CallID) > 256 {
				return history, "", nil
			}
			toolIDs = observe.AddToolID(toolIDs, parsed[i].CallID)
			if parsed[i].CallID == "" {
				return history, "", nil
			}
			i--
		}
		if len(toolIDs) == 0 {
			return history, "", nil
		}
		for i >= 0 && parsed[i].Type == "function_call" {
			expected = observe.AddToolID(expected, parsed[i].CallID)
			i--
		}
		i++
	}
	for _, id := range toolIDs {
		if !observe.Reported(expected, id) {
			return history, "", nil
		}
	}
	previous = historyHash(path, rows[:i], context)
	return
}

type sessionAdapter struct {
	Client  string   `json:"client"`
	Sources []string `json:"sources"`
	Note    string   `json:"note"`
}

func sessionAdapters() []sessionAdapter {
	return []sessionAdapter{
		{"通用客户端 / 自建智能体", []string{"X-Session-ID", "metadata.session_id"}, "每个对话使用唯一且稳定的 ID；X-Session-ID 优先。"},
		{"Claude Code", []string{"X-Claude-Code-Session-Id"}, "仅识别实际收到的会话头；不按用户 ID 合并。"},
		{"OpenCode", []string{"X-OpenCode-Session"}, "按原生会话 ID 归档；不同客户端命名空间隔离。"},
		{"Codex / 兼容客户端", []string{"thread-id", "thread_id", "session-id", "session_id"}, "thread 标识优先；部分版本的 session 标识用于缓存亲和。"},
		{"DSH", []string{"X-DeepSeek-Harness-Session-Id", "dsh_session_log.session.id"}, "是否发送取决于适配器；当前自定义 OpenAI 路径可能缺失。"},
		{"Pi / 兼容客户端", []string{"X-Session-Affinity", "metadata.user_id → session_id"}, "只提取结构化 JSON 内的 session_id；缓存键、请求 ID 不作为会话。"},
	}
}
