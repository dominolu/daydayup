package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"safuno/internal/observe"
	"safuno/internal/sessions"
)

func TestNativeSessionIdentity(t *testing.T) {
	for _, tc := range []struct {
		name             string
		headers          map[string]string
		body, id, source string
	}{
		{"portable", map[string]string{"X-Session-ID": "chat-1", "X-OpenCode-Session": "other"}, `{}`, "chat-1", "header:x-session-id"},
		{"opencode", map[string]string{"X-OpenCode-Session": "ses_one"}, `{}`, "opencode:ses_one", "header:x-opencode-session"},
		{"claude", map[string]string{"X-Claude-Code-Session-Id": "uuid"}, `{}`, "claude-code:uuid", "header:x-claude-code-session-id"},
		{"dsh", map[string]string{"X-DeepSeek-Harness-Session-Id": "uuid"}, `{}`, "dsh:uuid", "header:x-deepseek-harness-session-id"},
		{"codex old", map[string]string{"Session_id": "uuid"}, `{}`, "compat:uuid", "header:session_id"},
		{"codex new", map[string]string{"Session-Id": "uuid"}, `{}`, "compat:uuid", "header:session-id"},
		{"codex actual thread", map[string]string{"Thread-Id": "thread-1", "Session-Id": "shared-cache", "X-Session-Affinity": "cache"}, `{}`, "compat:thread-1", "header:thread-id"},
		{"codex legacy thread", map[string]string{"Thread_id": "thread-1"}, `{}`, "compat:thread-1", "header:thread_id"},
		{"same aliases", map[string]string{"Session_id": "uuid", "Session-Id": "uuid"}, `{}`, "compat:uuid", "header:session-id"},
		{"wrapper", map[string]string{"X-OpenCode-Session": "uuid", "X-DeepSeek-Harness-Session-Id": "uuid"}, `{}`, "opencode:uuid", "header:x-opencode-session"},
		{"pi", map[string]string{"X-Session-Affinity": "uuid"}, `{}`, "compat:uuid", "header:x-session-affinity"},
		{"metadata", nil, `{"metadata":{"session_id":"chat-1"}}`, "chat-1", "body:metadata.session_id"},
		{"dsh body extension", nil, `{"dsh_session_log":{"session":{"id":"uuid"}}}`, "dsh:uuid", "body:dsh_session_log.session.id"},
		{"structured user metadata", nil, `{"metadata":{"user_id":"{\"device_id\":\"not-a-session\",\"session_id\":\"uuid\"}"}}`, "compat:uuid", "body:metadata.user_id.session_id"},
		{"weak signals", map[string]string{"X-Request-ID": "uuid", "X-Client-Request-ID": "uuid", "X-DeepSeek-Harness-User-Id": "uuid", "User-Agent": "deepseek-harness/1", "X-Forwarded-For": "127.0.0.1"}, `{"user":"uuid","prompt_cache_key":"uuid","metadata":{"user_id":"uuid"}}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			id, identity, err := resolveIdentity(r, []byte(tc.body))
			if err != nil || id != tc.id || identity.Source != tc.source {
				t.Fatalf("%q %+v %v", id, identity, err)
			}
		})
	}
	for _, values := range [][]string{{"a", "b"}, {"a,b"}, {""}, {strings.Repeat("x", 513)}, {"a\nb"}} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header["X-Opencode-Session"] = values
		if _, _, err := resolveIdentity(r, nil); err == nil {
			t.Fatalf("accepted invalid native header %q", values)
		}
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Session_id", "a")
	r.Header.Set("Session-Id", "b")
	if _, _, err := resolveIdentity(r, nil); err == nil {
		t.Fatal("conflicting native identities accepted")
	}
	value := strings.Repeat("中", 100)
	a, b := nativeSessionID("opencode", value), nativeSessionID("dsh", value)
	if a == b || !sessions.ValidID(a) || a != nativeSessionID("opencode", value) {
		t.Fatal("native normalization lost stable namespace")
	}
	if a == nativeSessionID("opencode", strings.TrimPrefix(a, "opencode:")) {
		t.Fatal("raw ID collided with hashed representation")
	}
}

func identityCall(s *Server, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

const toolRequest = `{"model":"m","stream":true,"messages":[{"role":"user","content":"private question"}]}`
const toolFollowup = `{"model":"m","messages":[{"content":"private question","role":"user"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_unique_1","type":"function","function":{"name":"bash","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_unique_1","content":"private result"}]}`

func toolUpstream(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	if strings.Contains(string(b), "tool_call_id") {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sessionResponse)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_unique_1\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n")
}

func TestHeaderlessDSHToolContinuationSurvivesRestart(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(toolUpstream), 2)
	enableSessions(t, s)
	logs := captureLogs(s)
	h := map[string]string{"User-Agent": "deepseek-harness/0.2.0"}
	first := identityCall(s, "/v1/chat/completions", toolRequest, h)
	id := first.Header().Get("X-Session-ID")
	if first.Code != 200 || first.Header().Get("X-Session-Source") != "generated" {
		t.Fatalf("first %d %s", first.Code, first.Body)
	}
	// The link evidence is encrypted durable metadata, not process-local memory.
	store, err := sessions.Open(s.Config.DBFile, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Sessions = store
	second := identityCall(s, "/v1/chat/completions", toolFollowup, h)
	if second.Code != 200 || second.Header().Get("X-Session-ID") != id || second.Header().Get("X-Session-Source") != "tool_continuation" {
		t.Fatalf("second %d %s %+v", second.Code, second.Body, second.Header())
	}
	sum, rows, total, err := s.Sessions.Detail(id, 0, 30)
	if err != nil || total != 2 || sum.Usage.Total != 20 || sum.LinkedRequests != 1 || sum.GeneratedRequests != 1 || rows[0].AccountID != rows[1].AccountID || rows[0].Identity.LinkedRequestID != rows[1].ID || rows[0].Identity.Client != "DSH" {
		t.Fatalf("not one sticky conversation: %+v %+v %v", sum, rows, err)
	}
	if !strings.Contains(logs.String(), `"session_source":"tool_continuation"`) || strings.Contains(logs.String(), "private question") || strings.Contains(logs.String(), "private result") {
		t.Fatal("logs lack provenance or leak contents")
	}
	for name, body := range map[string]string{
		"changed history":         strings.Replace(toolFollowup, "private question", "different question", 1),
		"changed tool id":         strings.ReplaceAll(toolFollowup, "call_unique_1", "call_unknown"),
		"identical first request": toolRequest,
	} {
		t.Run(name, func(t *testing.T) {
			w := identityCall(s, "/v1/chat/completions", body, h)
			if w.Code != 200 || w.Header().Get("X-Session-ID") == id {
				t.Fatalf("unrelated request merged: %d %+v", w.Code, w.Header())
			}
		})
	}
	// Another explicit conversation wins even when its history is identical.
	explicit := identityCall(s, "/v1/chat/completions", toolFollowup, map[string]string{"X-Session-ID": "new-conversation"})
	if explicit.Header().Get("X-Session-ID") != "new-conversation" {
		t.Fatal("explicit conversation overridden")
	}
}

func TestAmbiguousToolLinksAreNotMerged(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(toolUpstream), 1)
	enableSessions(t, s)
	for _, id := range []string{"one", "two"} {
		w := sessionCall(s, id, "/v1/chat/completions", toolRequest)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	w := sessionCall(s, "", "/v1/chat/completions", toolFollowup)
	if id := w.Header().Get("X-Session-ID"); id == "one" || id == "two" {
		t.Fatal("ambiguous tool link chosen")
	}
	_, rows, _, _ := s.Sessions.Detail(w.Header().Get("X-Session-ID"), 0, 30)
	if rows[0].Identity.Note != "ambiguous_tool_continuation" {
		t.Fatalf("missing ambiguity reason %+v", rows[0].Identity)
	}
	history, prev, ids := requestHistory("/v1/chat/completions", []byte(toolFollowup))
	if history == "" || prev == "" || len(ids) != 1 {
		t.Fatal("missing evidence")
	}
	if sid, _, _ := s.Sessions.ResolveToolContinuation("other", "/v1/chat/completions", prev, ids); sid != "" {
		t.Fatal("cross-provider match")
	}
	if sid, _, _ := s.Sessions.ResolveToolContinuation("sensenova", "/v1/messages", prev, ids); sid != "" {
		t.Fatal("cross-protocol match")
	}
}

func TestCodexOpaqueStateRequiresKnownAccountAndDoesNotFailOver(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer upstream-secret-0" {
			w.Header().Set("X-Codex-Turn-State", "state-owned-a")
		} else {
			w.Header().Set("X-Codex-Turn-State", "state-owned-b")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sessionResponse)
	}), 2)
	enableSessions(t, s)
	headers := map[string]string{"Thread-Id": "thread-a", "Session-Id": "cache-shared"}
	first := identityCall(s, "/v1/responses", `{"model":"m","input":"hi"}`, headers)
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body)
	}
	account := s.Sessions.PreferredAccount("compat:thread-a", "m")
	if err := s.Pool.SetEnabled(account, false); err != nil {
		t.Fatal(err)
	}
	// A stateless fallback is allowed to move the preference, but the old opaque
	// token must still resolve to its issuing account rather than that preference.
	other := identityCall(s, "/v1/responses", `{"model":"m","input":"stateless"}`, headers)
	if other.Code != 200 || s.Sessions.PreferredAccount("compat:thread-a", "m") == account {
		t.Fatal("fixture did not move the preferred account")
	}
	store, err := sessions.Open(s.Config.DBFile, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Sessions = store
	headers["X-Codex-Turn-State"] = "state-owned-a"
	w := identityCall(s, "/v1/responses", `{"model":"m","input":"next"}`, headers)
	if w.Code != 503 {
		t.Fatalf("opaque state failed over: %d %s", w.Code, w.Body)
	}
	headers["Thread-Id"] = "unknown-thread"
	headers["X-Codex-Turn-State"] = "unknown-opaque-state"
	w = identityCall(s, "/v1/responses", `{"model":"m","input":"next"}`, headers)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "unknown_upstream_state") {
		t.Fatal("unknown state routed", w.Code, w.Body)
	}
}

func TestNativeHeadersArchiveForwardingAndGatewayAuth(t *testing.T) {
	body := `{"model":"m","unknown_extension":{"keep":true}}`
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != body || r.Header.Get("X-OpenCode-Session") != "native-1" || r.Header.Get("X-Session-Source") != "" {
			t.Error("wire request modified")
		}
		w.Header().Set("X-Session-Source", "untrusted")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sessionResponse)
	}), 1)
	enableSessions(t, s)
	w := identityCall(s, "/v1/chat/completions", body, map[string]string{"X-OpenCode-Session": "native-1"})
	if w.Code != 200 || w.Header().Get("X-Session-Source") != "header:x-opencode-session" {
		t.Fatal(w.Code, w.Header())
	}
	_, rows, _, _ := s.Sessions.Detail("opencode:native-1", 0, 30)
	if rows[0].Request.Headers["X-OpenCode-Session"] != "native-1" || rows[0].Request.Headers["Authorization"] != "" {
		t.Fatal("header audit missing or leaks credential")
	}
	r := httptest.NewRequest("GET", "/admin/v1/session-adapters", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatal("adapter API unauthenticated")
	}
	res := adminCall(s, "GET", "/admin/v1/session-adapters", "")
	var adapters struct{ Data []sessionAdapter }
	_ = json.Unmarshal(res.Body.Bytes(), &adapters)
	if res.Code != 200 || len(adapters.Data) < 6 {
		t.Fatal("adapter registry missing")
	}
}

func TestProtocolToolCallEvidence(t *testing.T) {
	for _, tc := range []struct{ path, request, next, response string }{
		{"/v1/chat/completions", toolRequest, toolFollowup, `{"choices":[{"message":{"tool_calls":[{"id":"call_unique_1"}]}}]}`},
		{"/v1/messages", `{"messages":[{"role":"user","content":"hi"}]}`, `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"call_unique_1"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_unique_1","content":"ok"}]}]}`, `{"type":"content_block_start","content_block":{"type":"tool_use","id":"call_unique_1"}}`},
		{"/v1/responses", `{"input":[{"role":"user","content":"hi"}]}`, `{"input":[{"role":"user","content":"hi"},{"type":"function_call","call_id":"call_unique_1"},{"type":"function_call_output","call_id":"call_unique_1","output":"ok"}]}`, `{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"call_unique_1"}]}}`},
	} {
		h, _, _ := requestHistory(tc.path, []byte(tc.request))
		_, prev, ids := requestHistory(tc.path, []byte(tc.next))
		if h == "" || h != prev || strings.Join(ids, ",") != "call_unique_1" {
			t.Fatalf("%s invalid history %q %q %v", tc.path, h, prev, ids)
		}
		o := observe.NewObserver(false, false)
		o.Write([]byte(tc.response))
		o.Done()
		if strings.Join(o.ToolCallIDs(), ",") != "call_unique_1" {
			t.Fatalf("%s missing calls %v", tc.path, o.ToolCallIDs())
		}
	}
}
