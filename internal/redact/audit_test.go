package redact

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRestoreEveryTokenSplit(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	masked := mustMask(t, e, "before user@example.com after")
	for split := 0; split <= len(masked); split++ {
		s := e.NewRestoreSession()
		a, err := s.Push(context.Background(), []byte(masked[:split]))
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.Push(context.Background(), []byte(masked[split:]))
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.Finish(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := string(a) + string(b) + string(c); got != "before user@example.com after" {
			t.Fatalf("split %d: %q", split, got)
		}
	}
}

func TestNestedJSONArgumentsRoundtrip(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomWords = map[string][]string{"QUOTED": {"内\"容\\路径"}}
	e := newTestEngine(t, cfg)
	args, _ := json.Marshal(map[string]string{"email": "hello\nuser@example.com", "note": "内\"容\\路径\n下一行"})
	body, _ := json.Marshal(map[string]any{"tool_calls": []any{map[string]any{"function": map[string]string{"arguments": string(args)}}}})
	masked, _, err := e.MaskCount(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ToolCalls []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
	}
	if json.Unmarshal(masked, &doc) != nil || !json.Valid([]byte(doc.ToolCalls[0].Function.Arguments)) {
		t.Fatalf("masked nested JSON is invalid: %s", masked)
	}
	restored, err := e.RestoreJSON(context.Background(), masked)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, restored) {
		t.Fatalf("nested roundtrip: %s != %s", restored, body)
	}
}

func TestRestoreJSONEscapedPlaceholder(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	token := mustMask(t, e, "user@example.com")
	encoded := strings.ReplaceAll(token, "{", `\u007b`)
	out, err := e.RestoreJSON(context.Background(), []byte(`{"content":"`+encoded+`"}`))
	if err != nil || string(out) != `{"content":"user@example.com"}` {
		t.Fatalf("%s, %v", out, err)
	}
}

func TestFlushRetriesAfterFailure(t *testing.T) {
	cfg := testConfig(t)
	e := newTestEngine(t, cfg)
	token := mustMask(t, e, "user@example.com")
	// Make the database read-only for a while: the flush must fail without
	// losing the in-memory mapping, then succeed once writes are allowed.
	if _, err := e.store.db.SQL().Exec("PRAGMA query_only = ON"); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err == nil {
		t.Fatal("expected save failure")
	}
	if _, err := e.store.db.SQL().Exec("PRAGMA query_only = OFF"); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	fresh := newTestEngine(t, cfg)
	got, err := fresh.Restore(context.Background(), []byte(token))
	if err != nil || string(got) != "user@example.com" {
		t.Fatalf("mapping lost after retry: %s, %v", got, err)
	}
}

func TestCapacityDoesNotEvictLiveMappings(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxMappings = 1
	e := newTestEngine(t, cfg)
	first := mustMask(t, e, "first@example.com")
	if _, err := e.Mask(context.Background(), []byte("second@example.com")); err == nil {
		t.Fatal("capacity must reject rather than invalidate an in-flight token")
	}
	out, err := e.Restore(context.Background(), []byte(first))
	if err != nil || string(out) != "first@example.com" {
		t.Fatalf("live mapping lost: %s, %v", out, err)
	}
}

func TestStoreRefusesCorruptOverwrite(t *testing.T) {
	// A corrupted database file must fail closed at startup, never be silently
	// re-initialized (which would discard every mapping).
	cfg := testConfig(t)
	if err := os.MkdirAll(filepath.Dir(cfg.StoreFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.StoreFile, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("corrupted store accepted")
	}
	got, _ := os.ReadFile(cfg.StoreFile)
	if string(got) != "corrupt" {
		t.Fatal("corrupt store overwritten")
	}
}

func TestStoreValidatesKeyBeforeFirstRequest(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("GATEWAY_DB_KEY", "invalid")
	if _, err := New(cfg); err == nil {
		t.Fatal("invalid store key must fail startup even when store is absent")
	}
}

func TestExistingEmptyStoreFailsClosed(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(filepath.Dir(cfg.StoreFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.StoreFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("empty existing store must be treated as corruption")
	}
}

func TestMaskIsIdempotentWithCustomLabels(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomWords = map[string][]string{"CUSTOM": {"EMAIL"}}
	e := newTestEngine(t, cfg)
	first := mustMask(t, e, "user@example.com")
	second := mustMask(t, e, first)
	if first != second {
		t.Fatalf("placeholder masked again: %s -> %s", first, second)
	}
}

func TestPrivateKeyBlocksDoNotEatInterveningText(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	block := "-----BEGIN PRIVATE KEY-----\nABCDEFGHIJKLMNOPQRSTUVWXYZ\n-----END PRIVATE KEY-----"
	out, n, err := e.MaskCount(context.Background(), []byte(block+" keep this sentence "+block))
	if err != nil || n != 2 || !strings.Contains(string(out), " keep this sentence ") {
		t.Fatalf("private keys consumed unrelated content: %s n=%d err=%v", out, n, err)
	}
}

func TestCommonSKKeyAndRestoreQuotedToolArguments(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomWords = map[string][]string{"QUOTED": {`a"b\c`}}
	e := newTestEngine(t, cfg)
	key := mustMask(t, e, "sk-abcdefghijklmnopqrstuvwxyz123456")
	if !strings.HasPrefix(key, "{{API_KEY_") {
		t.Fatalf("key not masked: %s", key)
	}
	token := mustMask(t, e, `a"b\c`)
	args, _ := json.Marshal(map[string]string{"note": token})
	body, _ := json.Marshal(map[string]string{"arguments": string(args)})
	out, err := e.RestoreJSON(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	var outer map[string]string
	var inner map[string]string
	if json.Unmarshal(out, &outer) != nil || json.Unmarshal([]byte(outer["arguments"]), &inner) != nil || inner["note"] != `a"b\c` {
		t.Fatalf("quoted tool arguments corrupted: %s", out)
	}
}

func TestExpiredMappingDoesNotReviveAfterRestart(t *testing.T) {
	cfg := testConfig(t)
	cfg.TokenTTLSeconds = 1
	e := newTestEngine(t, cfg)
	token := mustMask(t, e, "user@example.com")
	e.tm.mu.Lock()
	entry := e.tm.reverse[token]
	entry.touched = entry.touched.Add(-2 * time.Hour)
	e.tm.reverse[token] = entry
	e.tm.mu.Unlock()
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	fresh := newTestEngine(t, cfg)
	got, err := fresh.Restore(context.Background(), []byte(token))
	if err != nil || string(got) != token {
		t.Fatalf("expired mapping revived: %s %v", got, err)
	}
}

func FuzzJSONMaskRestoreRoundtrip(f *testing.F) {
	cfg := Config{Enabled: true, StoreFile: filepath.Join(f.TempDir(), "map.enc")}
	e, err := New(cfg)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = e.Close() })
	for _, seed := range []string{"hello\nuser@example.com", "quote \" slash \\ tab\t13800138000", "nested {{literal}}", "密码：s3cret123", "user\u0040example.com"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 4096 || placeholderRE.MatchString(value) {
			t.Skip()
		}
		body, _ := json.Marshal(map[string]string{"content": value})
		masked, _, err := e.MaskCount(context.Background(), body)
		if err != nil {
			t.Skip()
		}
		if !json.Valid(masked) {
			t.Fatalf("invalid JSON: %s", masked)
		}
		restored, err := e.RestoreJSON(context.Background(), masked)
		if err != nil {
			t.Fatal(err)
		}
		var want, got map[string]string
		json.Unmarshal(body, &want)
		json.Unmarshal(restored, &got)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("roundtrip differs: input=%q restored=%q", want["content"], got["content"])
		}
	})
}

func TestRequestSchemasDoNotSuppressArgumentMasking(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomWords = map[string][]string{"CUSTOM": {"secret"}}
	e := newTestEngine(t, cfg)
	body := []byte(`{"model":"secret","conversation":{"id":"secret"},"messages":[{"role":"user","content":[{"type":"tool_use","id":"call_1","name":"secret","input":{"name":"secret","role":"secret","function":{"name":"secret"}}}]}],"tools":[{"type":"function","function":{"name":"secret","parameters":{"type":"object","properties":{"role":{"type":"string","enum":["secret"],"description":"secret"}}}}}],"response_format":{"type":"json_schema","json_schema":{"name":"secret","schema":{"type":"string","enum":["secret"]}}}}`)
	out, n, err := e.MaskRequestCount(context.Background(), body)
	if err != nil || n != 4 || !json.Valid(out) {
		t.Fatalf("request policy failed: n=%d err=%v out=%s", n, err, out)
	}
	if !strings.Contains(string(out), `"model":"secret"`) || !strings.Contains(string(out), `"name":"secret","input":{"name":"{{CUSTOM_`) || strings.Count(string(out), `"enum":["secret"]`) != 2 {
		t.Fatalf("schema or input changed incorrectly: %s", out)
	}
}

func TestLiteralTokenExamplesCanRoundtripThroughTools(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	original := `write {{EMAIL_zzzzzz}} as a literal example`
	masked := mustMask(t, e, original)
	if strings.Contains(masked, "{{EMAIL_zzzzzz}}") || !strings.Contains(masked, "{{LITERAL_") {
		t.Fatalf("literal example not distinguished: %s", masked)
	}
	again := mustMask(t, e, masked)
	if again != masked {
		t.Fatalf("masking is not idempotent: %s", again)
	}
	s := e.NewRestoreSessionJSON()
	out, err := s.Push(context.Background(), []byte(masked))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := s.Finish(context.Background())
	if err != nil || string(out)+string(tail) != original {
		t.Fatalf("literal example failed roundtrip: %s %v", out, err)
	}
}
