package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"safuno/internal/redact"
)

func TestRedactionPreservesProtocolAndMedia(t *testing.T) {
	s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "model-secret" || body["metadata"].(map[string]any)["session_id"] != "session-12345678" {
			t.Errorf("routing changed: %+v", body)
		}
		messages := body["messages"].([]any)
		parts := messages[0].(map[string]any)["content"].([]any)
		if parts[0].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,model-secret" {
			t.Fatal("media changed")
		}
		if parts[1].(map[string]any)["text"] == "model-secret user@example.com" {
			t.Fatal("content not masked")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	cfg := *s.redactionConfig()
	cfg.CustomWords = map[string][]string{"CUSTOM": {"model-secret"}}
	if err := s.redact.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"model-secret","metadata":{"session_id":"session-12345678"},"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,model-secret"}},{"type":"text","text":"model-secret user@example.com"}]}]}`
	r := call(s, "POST", "/v1/chat/completions", body)
	if r.Code != 200 || r.Header().Get("X-Session-ID") != "session-12345678" {
		t.Fatalf("session routing failed: %d %s", r.Code, r.Body.String())
	}
}

func TestRedactionMappingSurvivesGatewayRecreation(t *testing.T) {
	s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"user@example.com"}]}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	fresh, err := redact.New(*s.redactionConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if fresh.Stats().Mappings == 0 {
		t.Fatal("gateway never persisted mappings")
	}
	token, err := s.redact.Mask(context.Background(), []byte("user@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := fresh.Restore(context.Background(), token)
	if err != nil || string(restored) != "user@example.com" {
		t.Fatalf("restart lost mapping: %s %v", restored, err)
	}
}

func TestRedactionPersistenceFailureRejectsBeforeUpstream(t *testing.T) {
	calls := 0
	s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	if err := s.redact.SetStoreReadOnly(true); err != nil {
		t.Fatal(err)
	}
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"user@example.com"}]}`)
	if rec.Code != 500 || calls != 0 {
		t.Fatalf("persistence failure leaked upstream: status=%d calls=%d", rec.Code, calls)
	}
}

func TestRedactionRequestGzipAndForm(t *testing.T) {
	for _, kind := range []string{"gzip", "form"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if strings.Contains(string(b), "user@example.com") || strings.Contains(string(b), "s3cret123") {
					t.Errorf("unmasked body: %s", b)
				}
				if r.Header.Get("Content-Encoding") != "" {
					t.Error("obsolete gzip header forwarded")
				}
				if kind == "gzip" && !json.Valid(b) {
					t.Errorf("gzip body not decoded as JSON: %s", b)
				}
				if kind == "form" {
					r.Body = io.NopCloser(bytes.NewReader(b))
					if err := r.ParseForm(); err != nil || r.Form.Get("model") != "m" || !strings.Contains(r.Form.Get("password"), "{{SECRET_") {
						t.Errorf("invalid masked form: %v %v", r.Form, err)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"ok":true}`)
			}))
			var payload []byte
			ct := "application/json"
			encoding := ""
			if kind == "gzip" {
				var b bytes.Buffer
				z := gzip.NewWriter(&b)
				z.Write([]byte(`{"model":"m","messages":[{"content":"hello\nuser@example.com"}]}`))
				z.Close()
				payload = b.Bytes()
				encoding = "gzip"
			} else {
				payload = []byte("model=m&password=s3cret123&email=user%40example.com")
				ct = "application/x-www-form-urlencoded"
			}
			r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(payload))
			r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
			r.Header.Set("Content-Type", ct)
			r.Header.Set("Content-Encoding", encoding)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRedactionResponseLengthAndNestedArguments(t *testing.T) {
	s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct{ Messages []struct{ Content string } }
		json.NewDecoder(r.Body).Decode(&input)
		args, _ := json.Marshal(map[string]string{"note": input.Messages[0].Content})
		body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"arguments": string(args)}}}}}}})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}))
	cfg := *s.redactionConfig()
	cfg.CustomWords = map[string][]string{"QUOTED": {`a"b\c`}}
	if err := s.redact.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"a\"b\\c"}]}`)
	if rec.Code != 200 || rec.Header().Get("Content-Length") != "" || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("invalid restored response: %d %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
			}
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(doc.Choices[0].Message.ToolCalls[0].Function.Arguments), &args); err != nil || args["note"] != `a"b\c` {
		t.Fatalf("nested arguments corrupted: %v %v", args, err)
	}
}

func TestRedactionResponseOverLimitAbortsWithoutTruncation(t *testing.T) {
	cfg := redactionTestConfig(t, t.TempDir())
	cfg.MaxRestoreBuffer = 32
	e, err := redact.New(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := &Server{redact: e}
	w := httptest.NewRecorder()
	if err := s.copyRestored(context.Background(), w, strings.NewReader(`{"content":"`+strings.Repeat("a", 100)+`"}`), true); err == nil || w.Body.Len() != 0 {
		t.Fatalf("oversize response was silently truncated: %s %v", w.Body.String(), err)
	}
}

func TestRedactionRulesConcurrentWithRequests(t *testing.T) {
	s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := 0; n < 15; n++ {
				switch worker {
				case 0:
					r := httptest.NewRequest("PUT", "/admin/v1/redaction/rules", strings.NewReader(`{"custom_words":{"CUSTOM":["private-project"]}}`))
					r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
					s.ServeHTTP(httptest.NewRecorder(), r)
				case 1:
					r := httptest.NewRequest("GET", "/admin/v1/redaction", nil)
					r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
					s.ServeHTTP(httptest.NewRecorder(), r)
				default:
					rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"content":"user@example.com private-project"}]}`)
					if rec.Code != 200 {
						t.Errorf("worker %d status %d: %s", worker, rec.Code, rec.Body.String())
					}
				}
			}
		}(i)
	}
	wg.Wait()
	if _, err := os.Stat(s.redactionConfig().StoreFile); err != nil {
		t.Fatal(fmt.Errorf("no persistent mappings: %w", err))
	}
}

func TestRedactionGzipAndPlainResponses(t *testing.T) {
	for _, kind := range []string{"gzip-json", "plain"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct{ Messages []struct{ Content string } }
				json.NewDecoder(r.Body).Decode(&input)
				var body []byte
				if kind == "gzip-json" {
					body, _ = json.Marshal(map[string]string{"content": input.Messages[0].Content})
					var b bytes.Buffer
					z := gzip.NewWriter(&b)
					z.Write(body)
					z.Close()
					body = b.Bytes()
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Encoding", "gzip")
				} else {
					body = []byte(input.Messages[0].Content)
					w.Header().Set("Content-Type", "text/plain")
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.Write(body)
			}))
			rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"content":"user@example.com"}]}`)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "user@example.com") || rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Content-Length") != "" {
				t.Fatalf("response not restored: %d %s headers=%v", rec.Code, rec.Body.String(), rec.Header())
			}
		})
	}
}

func TestRedactionRejectsInvalidEncodedBodies(t *testing.T) {
	for _, tc := range []struct {
		body, encoding string
		status         int
	}{{"{}", "br", 415}, {"bad", "gzip", 400}, {`{"content":`, "", 400}} {
		calls := 0
		s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Content-Encoding", tc.encoding)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status || calls != 0 {
			t.Fatalf("invalid body reached upstream: status=%d calls=%d", w.Code, calls)
		}
	}
}

func TestRedactionFailOpenKeepsCompressedBodyConsistent(t *testing.T) {
	var raw bytes.Buffer
	z := gzip.NewWriter(&raw)
	z.Write([]byte(`{"model":"m","messages":[{"content":"user@example.com"}]}`))
	z.Close()
	s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !bytes.Equal(b, raw.Bytes()) || r.Header.Get("Content-Encoding") != "gzip" {
			t.Error("fail-open changed original body or encoding")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	disabled := false
	s.Config.Redaction.FailClosedValue = false
	s.Config.Redaction.FailClosed = &disabled
	if err := s.redact.SetStoreReadOnly(true); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw.Bytes()))
	r.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestRedactionNonStreamingToolGuard(t *testing.T) {
	s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"function":{"arguments":"{\"cmd\":\"rm -rf /tmp/example\"}"}}]}}]}`)
	}))
	cfg := *s.redactionConfig()
	cfg.ToolGuard = &redact.ToolGuard{Enabled: true, Mode: redact.ToolGuardObserve}
	if err := s.redact.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"content":"test"}]}`)
	if rec.Code != 200 || s.redact.Stats().ToolFindings != 1 {
		t.Fatalf("non-stream tool guard missed call: %d %+v", rec.Code, s.redact.Stats())
	}
}

func TestRedactionRulesSaveAndFailedSaveAreAtomic(t *testing.T) {
	s, _ := redactedGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	path := filepath.Join(t.TempDir(), "config.json")
	initial := `{"instance_name":"unchanged","accounts":[{"api_key_env":"SECRET_ENV_NAME"}],"redaction":{"enabled":true}}`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	s.Config.SourcePath = path
	update := func(value string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/admin/v1/redaction/rules", strings.NewReader(value))
		r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	w := update(`{"custom_words":{"CUSTOM":["private-project"]}}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	raw, _ := os.ReadFile(path)
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if string(stored["instance_name"]) != `"unchanged"` || !strings.Contains(string(stored["accounts"]), "SECRET_ENV_NAME") || !strings.Contains(string(stored["redaction"]), "private-project") {
		t.Fatalf("config not safely saved: %s", raw)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	w = update(`{"custom_words":{"OTHER":["different-project"]}}`)
	if w.Code != 500 || s.redactionConfig().CustomWords["CUSTOM"][0] != "private-project" {
		t.Fatalf("failed save changed live rules: %d %+v", w.Code, s.redactionConfig())
	}
}
