package observe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"safuno/internal/redact"
)

func sseRestoreEngine(t *testing.T) *redact.Engine {
	t.Helper()
	os.Setenv("GATEWAY_REDACTION_STORE_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Cleanup(func() { os.Unsetenv("GATEWAY_REDACTION_STORE_KEY") })
	cfg := redact.Config{
		Enabled:     true,
		StoreFile:   filepath.Join(t.TempDir(), "map.enc"),
		StoreKeyEnv: "GATEWAY_REDACTION_STORE_KEY",
	}
	e, err := redact.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func maskValue(t *testing.T, e *redact.Engine, label, value string) string {
	t.Helper()
	out, err := e.Mask(context.Background(), []byte(value))
	if err != nil {
		t.Fatalf("Mask: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "{{"+label+"_") {
		t.Fatalf("expected %s placeholder in %q", label, s)
	}
	return s[strings.Index(s, "{{") : strings.Index(s, "}}")+2]
}

func TestSSEChannelRestoresTextAcrossEvents(t *testing.T) {
	e := sseRestoreEngine(t)
	token := maskValue(t, e, "EMAIL", "user@example.com")
	r := newSSEChannelRestorer(context.Background(), e)

	// The placeholder arrives split across two deltas of the same channel.
	half := token[len(token)/2:]
	first := `{"choices":[{"index":0,"delta":{"content":"回信给 ` + token[:len(token)/2] + `"}}]}`
	out1, err := r.feed([]byte("data: " + first + "\n"))
	if err != nil {
		t.Fatalf("feed1: %v", err)
	}
	if strings.Contains(string(out1), "{{") {
		t.Fatalf("partial token must stay pending in feed1: %s", out1)
	}
	second := `{"choices":[{"index":0,"delta":{"content":"` + half + ` 谢谢"}}]}`
	out2, err := r.feed([]byte("data: " + second + "\n"))
	if err != nil {
		t.Fatalf("feed2: %v", err)
	}
	if !strings.Contains(string(out2), "user@example.com") {
		t.Fatalf("split token not restored in feed2: %s", out2)
	}
	if strings.Contains(string(out2)+string(out1), "{{") {
		t.Fatalf("placeholder leaked: %s%s", out1, out2)
	}
	if err := r.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestSSEChannelRestoresToolArgumentsWithEscaping(t *testing.T) {
	os.Setenv("GATEWAY_REDACTION_STORE_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	defer os.Unsetenv("GATEWAY_REDACTION_STORE_KEY")
	cfg := redact.Config{
		Enabled:     true,
		StoreFile:   filepath.Join(t.TempDir(), "map.enc"),
		StoreKeyEnv: "GATEWAY_REDACTION_STORE_KEY",
		CustomWords: map[string][]string{"QUOTED": {`a"b@example.com`}},
	}
	e, err := redact.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	token := maskValue(t, e, "QUOTED", `a"b@example.com`)
	r := newSSEChannelRestorer(context.Background(), e)

	// The restored value contains a double quote; it must be JSON-escaped so
	// the tool-call arguments payload stays valid JSON.
	payload := `{"id":"3","type":"response.output_item.delta","delta":{"partial_json":"{\"tag\":\"` + token + `\"}"}}`
	out, err := r.feed([]byte("data: " + payload + "\n"))
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if err := r.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	out = r.tails
	body := strings.TrimPrefix(string(out), "data: ")
	if strings.Contains(body, token) {
		t.Fatalf("placeholder leaked: %s", body)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("restored payload is not valid JSON: %v (%s)", err, body)
	}
	delta, _ := parsed["delta"].(map[string]any)
	args, _ := delta["partial_json"].(string)
	if args != `{"tag":"a\"b@example.com"}` {
		t.Fatalf("partial_json not restored with JSON escaping: %q", args)
	}
	if err := r.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestSSEChannelFailClosedOnUnresolvedToolArgument(t *testing.T) {
	e := sseRestoreEngine(t)
	r := newSSEChannelRestorer(context.Background(), e)
	payload := `{"id":"3","function":{"arguments":"{\"cmd\":\"{{NOPE_zzzzzz}}\"}"}}`
	out, err := r.feed([]byte("data: " + payload + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "{{NOPE_") {
		t.Fatal("unresolved arguments forwarded")
	}
	if err := r.finish(); err == nil {
		t.Fatal("unresolved tool call must fail before completion")
	}
	if !r.bad {
		t.Fatal("unresolved tool placeholder not flagged")
	}
}

func TestSSEChannelNonDataLinesPassThrough(t *testing.T) {
	e := sseRestoreEngine(t)
	r := newSSEChannelRestorer(context.Background(), e)
	for _, line := range []string{"event: message", "id: 7", "\n", "data: [DONE]\n"} {
		out, err := r.feed([]byte(line))
		if err != nil {
			t.Fatalf("feed %q: %v", line, err)
		}
		if string(out) != line {
			t.Fatalf("non-data line altered: %q -> %q", line, out)
		}
	}
	if err := r.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestSSEChannelLiteralBracesNotFailClosed(t *testing.T) {
	e := sseRestoreEngine(t)
	r := newSSEChannelRestorer(context.Background(), e)
	// Literal double braces in tool arguments (templates, JSON fragments) must
	// NOT be treated as unresolved placeholders.
	for _, payload := range []string{
		`{"id":"1","delta":{"partial_json":"{\"tpl\":\"{{var}}\"}"}}`,
		`{"id":"1","delta":{"partial_json":"{{\"a\":1}"}}`,
		`{"id":"1","delta":{"partial_json":"{\"t\":\"{{user_name}}\"}"}}`,
	} {
		if _, err := r.feed([]byte("data: " + payload + "\n")); err != nil {
			t.Fatalf("feed %s: %v", payload, err)
		}
		if r.bad {
			t.Fatalf("literal braces incorrectly flagged as unresolved: %s", payload)
		}
	}
	if err := r.finish(); err != nil {
		t.Fatalf("finish must succeed for literal braces: %v", err)
	}
}

func TestSSEChannelPartialTokenAtFinishFails(t *testing.T) {
	e := sseRestoreEngine(t)
	r := newSSEChannelRestorer(context.Background(), e)
	// A tool argument fragment that ends with a token prefix {{EMAIL_bc (a
	// vowel-free suffix fragment) with no continuation corrupts the tool call;
	// the stream must fail closed.
	payload := `{"id":"1","delta":{"partial_json":"{\"x\":\"{{EMAIL_bc"}}`
	if _, err := r.feed([]byte("data: " + payload + "\n")); err != nil {
		t.Fatalf("feed: %v", err)
	}
	if r.bad {
		t.Fatal("mid-stream partial must be held pending, not flagged yet")
	}
	if err := r.finish(); err == nil {
		t.Fatal("finish must fail for an unterminated token prefix in tool args")
	}
}

func TestSSEChannelInventedTokenStillFails(t *testing.T) {
	e := sseRestoreEngine(t)
	r := newSSEChannelRestorer(context.Background(), e)
	payload := `{"id":"1","delta":{"partial_json":"{\"cmd\":\"echo {{EMAIL_kwtvxd}}\"}"}}`
	out, err := r.feed([]byte("data: " + payload + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "{{EMAIL_") {
		t.Fatal("invented arguments forwarded")
	}
	if err := r.finish(); err == nil {
		t.Fatal("invented tool call must fail before completion")
	}
	if !r.bad {
		t.Fatal("invented placeholder not flagged")
	}
}
