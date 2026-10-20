package redact

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	os.Setenv("GATEWAY_REDACTION_STORE_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Cleanup(func() { os.Unsetenv("GATEWAY_REDACTION_STORE_KEY") })
	return Config{
		Enabled:     true,
		StoreFile:   filepath.Join(dir, "map.enc"),
		StoreKeyEnv: "GATEWAY_REDACTION_STORE_KEY",
	}
}

func newTestEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func mustMask(t *testing.T, e *Engine, body string) string {
	t.Helper()
	out, err := e.Mask(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("Mask: %v", err)
	}
	return string(out)
}

func TestMaskSensitiveValues(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		label string
	}{
		{"email", `请发到 user@example.com 谢谢`, "EMAIL"},
		{"phone", `联系电话 13800138000`, "PHONE"},
		{"phone-separated", `138 0013 8000`, "PHONE"},
		{"idcard", `身份证 11010519491231002X`, "IDCARD"},
		{"card", `卡号 4111111111111111`, "CARD"},
		{"jwt", `token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.signature123`, "JWT"},
		{"github-key", `key ghp_abcdefghijklmnopqrstuvwxyzABCD`, "API_KEY"},
		{"stripe-key", `sk_live_abcdefghijklmnopqrstuv`, "API_KEY"},
		{"aws-key", `AKIAIOSFODNN7EXAMPLE`, "ACCESS_KEY"},
		{"bearer", `Authorization: Bearer abc.def.ghi.jkl.mno.pqr.1234567890`, "TOKEN"},
		{"secret-en", `"password": "s3cret123"`, "SECRET"},
		{"secret-cn", `密码：s3cret123`, "SECRET"},
		{"connstr", `mysql://root:dbpass123@10.0.0.5:3306/db`, "CONNSTR"},
		{"url-query", `https://x.com/api?access_token=ab12cd34ef56`, "URL_QUERY_SECRET"},
		{"private-ip", `内网地址 192.168.1.10`, "IP_PRIVATE"},
		{"internal-ip", `地址 10.1.2.3`, "IP_INTERNAL"},
		{"cookie-session", `sessionid=abc12345def`, "COOKIE_SESSION"},
		{"landline", `电话 010-62345678`, "LANDLINE"},
		{"private-key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1ABCDEFGHIJKLMNOPQRSTUVWXYZ\n-----END RSA PRIVATE KEY-----", "PRIVATE_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, testConfig(t))
			out := mustMask(t, e, tc.in)
			if !strings.Contains(out, "{{"+tc.label+"_") {
				t.Fatalf("masked output %q missing %q placeholder", out, tc.label)
			}
			if strings.Contains(out, tc.in) {
				t.Fatalf("original text still present: %q", out)
			}
		})
	}
}

func TestMaskSkipsNonSensitive(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	body := `请总结这段代码：func Add(a, b int) int { return a + b }`
	out := mustMask(t, e, body)
	if out != body {
		t.Fatalf("non-sensitive body changed: %q", out)
	}
}

func TestMaskPreservesCredentialContext(t *testing.T) {
	cases := []struct {
		name, input, value, label string
	}{
		{"password", `"password": "s3cret123"`, "s3cret123", "SECRET"},
		{"bearer", `Authorization: Bearer abc.def.ghi.jkl.mno.pqr.1234567890`, "abc.def.ghi.jkl.mno.pqr.1234567890", "TOKEN"},
		{"connection", `mysql://root:dbpass123@db.example.test/database`, "dbpass123", "CONNSTR"},
		{"aws-secret", `AWS_SECRET_ACCESS_KEY="abcdefghijklmnopqrstuvwxyz1234567890abcd"`, "abcdefghijklmnopqrstuvwxyz1234567890abcd", "AWS_SECRET"},
		{"cookie", `Cookie: sessionid=abc12345def; theme=light`, "abc12345def", "COOKIE_SESSION"},
		{"basic", `Authorization: Basic dXNlcjpwYXNz, next`, "dXNlcjpwYXNz", "BASIC_AUTH"},
		{"query", `https://example.test/api?access_token=ab12cd34ef56&limit=10`, "ab12cd34ef56", "URL_QUERY_SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, testConfig(t))
			out, count, err := e.MaskCount(context.Background(), []byte(tc.input))
			if err != nil || count != 1 {
				t.Fatalf("MaskCount: count=%d, err=%v, output=%s", count, err, out)
			}
			token := placeholderRE.FindString(string(out))
			if !strings.HasPrefix(token, "{{"+tc.label+"_") {
				t.Fatalf("missing %s placeholder: %s", tc.label, out)
			}
			if want := strings.Replace(tc.input, tc.value, token, 1); string(out) != want {
				t.Fatalf("credential context changed: got %s, want %s", out, want)
			}
			restored, err := e.Restore(context.Background(), out)
			if err != nil || string(restored) != tc.input {
				t.Fatalf("credential roundtrip: %s, err=%v", restored, err)
			}
		})
	}
}

func TestMaskJSONPreservesEscapesAndStructure(t *testing.T) {
	cases := []struct {
		name, input, original string
	}{
		{"newline", `{"model":"m","messages":[{"role":"user","content":"hello\nuser@example.com"}],"max_tokens":123}`, "hello\nuser@example.com"},
		{"unicode-newline", `{"model":"m","messages":[{"role":"user","content":"\u4f60\u597d\nuser@example.com"}],"max_tokens":123}`, "你好\nuser@example.com"},
		{"tab", `{"model":"m","messages":[{"role":"user","content":"hello\tuser@example.com"}],"max_tokens":123}`, "hello\tuser@example.com"},
		{"escaped-email", `{"model":"m","messages":[{"role":"user","content":"user\u0040example.com"}],"max_tokens":123}`, "user@example.com"},
		{"credentials", `{"model":"m","messages":[{"role":"user","content":"password=s3cret123; mysql://root:dbpass123@db.example.test/db"}],"max_tokens":123}`, "password=s3cret123; mysql://root:dbpass123@db.example.test/db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, testConfig(t))
			out, count, err := e.MaskCount(context.Background(), []byte(tc.input))
			if err != nil || count == 0 {
				t.Fatalf("MaskCount: count=%d, err=%v", count, err)
			}
			var doc struct {
				Model     string                           `json:"model"`
				Messages  []struct{ Role, Content string } `json:"messages"`
				MaxTokens int                              `json:"max_tokens"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("masking produced invalid JSON: %v; %s", err, out)
			}
			if doc.Model != "m" || doc.MaxTokens != 123 || len(doc.Messages) != 1 || doc.Messages[0].Role != "user" {
				t.Fatalf("request structure changed: %s", out)
			}
			if !strings.Contains(doc.Messages[0].Content, "{{") || strings.Contains(doc.Messages[0].Content, "user@example.com") {
				t.Fatalf("sensitive content not masked: %s", out)
			}
			restored, err := e.RestoreJSON(context.Background(), out)
			if err != nil || json.Unmarshal(restored, &doc) != nil || doc.Messages[0].Content != tc.original {
				t.Fatalf("JSON roundtrip failed: %s, err=%v", restored, err)
			}
			stats := e.Stats()
			if stats.Requests != 1 || stats.Masked != uint64(count) {
				t.Fatalf("JSON request stats wrong: %+v", stats)
			}
		})
	}
}

func TestMaskJSONCredentialFieldsAndCustomPatterns(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomWords = map[string][]string{"CUSTOM": {"model", "role", "123", "re:^sensitive$"}}
	e := newTestEngine(t, cfg)
	body := ` { "model": "m", "role": "user", "max_tokens": 123, "password": "s3cret123", "content": "sensitive", "array": ["model", false, null, 123] } `
	out, count, err := e.MaskCount(context.Background(), []byte(body))
	if err != nil || count != 3 || !json.Valid(out) {
		t.Fatalf("MaskCount: count=%d, err=%v, output=%s", count, err, out)
	}
	if !strings.Contains(string(out), `"password": "{{SECRET_`) || strings.Count(string(out), "{{CUSTOM_") != 2 {
		t.Fatalf("credential or custom matches missing: %s", out)
	}
	if !strings.Contains(string(out), `"model": "m", "role": "user", "max_tokens": 123`) || !strings.Contains(string(out), `false, null, 123`) {
		t.Fatalf("JSON keys or non-string values changed: %s", out)
	}
	restored, err := e.RestoreJSON(context.Background(), out)
	if err != nil || string(restored) != body {
		t.Fatalf("JSON roundtrip: %s, err=%v", restored, err)
	}
}

func TestMaskSkipsPublicDNS(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	body := `使用 DNS 8.8.8.8 和 1.1.1.1`
	out := mustMask(t, e, body)
	if out != body {
		t.Fatalf("public DNS should not be masked: %q", out)
	}
}

func TestMaskInvalidCardSkips(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	body := `卡号 6222020202020201` // fails Luhn
	out := mustMask(t, e, body)
	if out != body {
		t.Fatalf("invalid Luhn card should not be masked: %q", out)
	}
}

func TestStablePlaceholderAndRoundtrip(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	first := mustMask(t, e, `联系 user@example.com 或 user@example.com`)
	second := mustMask(t, e, `联系 user@example.com`)
	// Both occurrences must share one token, and a later request the same.
	token := first[strings.Index(first, "{{") : strings.Index(first, "}}")+2]
	if strings.Count(first, token) != 2 {
		t.Fatalf("expected 2 occurrences of %s in %q", token, first)
	}
	if !strings.Contains(second, token) {
		t.Fatalf("second request should reuse %s, got %q", token, second)
	}
	// Roundtrip restore.
	restored, err := e.Restore(context.Background(), []byte(first))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if string(restored) != `联系 user@example.com 或 user@example.com` {
		t.Fatalf("roundtrip mismatch: %q", restored)
	}
}

func TestRestoreLeavesUnknownPlaceholders(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	body := `模板 {{EMAIL_abc}} 和 {{NOT_A_TOKEN}}`
	out, err := e.Restore(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if string(out) != body {
		t.Fatalf("unknown placeholders must stay: %q", out)
	}
}

func TestSuffixFallbackRestore(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	masked := mustMask(t, e, `邮箱 user@example.com`)
	token := masked[strings.Index(masked, "{{") : strings.Index(masked, "}}")+2]
	label := token[2:strings.LastIndexByte(token, '_')]
	suffix := token[strings.LastIndexByte(token, '_')+1 : len(token)-2]
	// Model echoes with different label casing.
	echoed := "{{" + strings.ToLower(label) + "_" + suffix + "}}"
	out, err := e.Restore(context.Background(), []byte(echoed))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if string(out) != `user@example.com` {
		t.Fatalf("suffix fallback failed: %q", out)
	}
}

func TestIncrementalRestoreAcrossChunks(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	masked := mustMask(t, e, `邮箱 user@example.com 完毕`)
	// Split the token across pushes at a mid-token boundary.
	split := strings.Index(masked, "{{") + 5
	rs := e.NewRestoreSession()
	part1, err := rs.Push(context.Background(), []byte(masked[:split]))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if strings.Contains(string(part1), "{{") {
		t.Fatalf("partial token must stay pending, got %q", part1)
	}
	part2, err := rs.Push(context.Background(), []byte(masked[split:]))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	final, err := rs.Finish(context.Background())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if string(part1)+string(part2)+string(final) != `邮箱 user@example.com 完毕` {
		t.Fatalf("incremental roundtrip mismatch: %q+%q+%q", part1, part2, final)
	}
}

func TestCustomWordsAndDisabledRules(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomWords = map[string][]string{
		"PROJECT_CODENAME": {"鹰眼系统", "re:[0-9]{4}-[A-Z]{2}-内部"},
	}
	cfg.DisabledRules = []string{"EMAIL"}
	e := newTestEngine(t, cfg)

	out := mustMask(t, e, `项目代号鹰眼系统 和 1234-AB-内部 已上线`)
	if !strings.Contains(out, "{{PROJECT_CODENAME_") {
		t.Fatalf("custom word not masked: %q", out)
	}
	if strings.Contains(out, "鹰眼系统") || strings.Contains(out, "1234-AB-内部") {
		t.Fatalf("custom original leaked: %q", out)
	}
	// Disabled EMAIL rule must not mask.
	email := mustMask(t, e, `邮箱 user@example.com`)
	if strings.Contains(email, "{{EMAIL_") {
		t.Fatalf("disabled EMAIL rule still masked: %q", email)
	}
}

func TestPersistenceAcrossInstances(t *testing.T) {
	cfg := testConfig(t)
	e := newTestEngine(t, cfg)
	masked := mustMask(t, e, `邮箱 user@example.com`)
	token := masked[strings.Index(masked, "{{") : strings.Index(masked, "}}")+2]
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// A fresh engine with the same store must resolve the old token.
	e2 := newTestEngine(t, cfg)
	out, err := e2.Restore(context.Background(), []byte(token))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if string(out) != `user@example.com` {
		t.Fatalf("persisted mapping failed: %q", out)
	}
}

func TestMappingStoreTamperDetected(t *testing.T) {
	cfg := testConfig(t)
	e := newTestEngine(t, cfg)
	if _, err := e.Mask(context.Background(), []byte(`邮箱 user@example.com`)); err != nil {
		t.Fatalf("Mask: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// Corrupt the encrypted mapping row in place: the outer AEAD (scoped to
	// the row) must reject it on the next load.
	if _, err := e.store.db.SQL().Exec("UPDATE redact_rules SET rules = 'tampered' WHERE scope = 'default'"); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("tampered store must fail to load")
	}
}

func TestConcurrentMaskAndRestore(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				masked, err := e.Mask(context.Background(), []byte(`联系 user@example.com 或 13800138000`))
				if err != nil {
					t.Errorf("Mask: %v", err)
					return
				}
				restored, err := e.Restore(context.Background(), masked)
				if err != nil {
					t.Errorf("Restore: %v", err)
					return
				}
				if string(restored) != `联系 user@example.com 或 13800138000` {
					t.Errorf("roundtrip mismatch: %q", restored)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestToolGuardObserve(t *testing.T) {
	cfg := testConfig(t)
	cfg.ToolGuard = &ToolGuard{Enabled: true, Mode: "observe"}
	e := newTestEngine(t, cfg)
	if findings := e.CheckToolArgs(`{"cmd":"rm -rf /"}`); len(findings) == 0 {
		t.Fatal("rm -rf must be flagged")
	}
	if findings := e.CheckToolArgs(`{"cmd":"echo hello"}`); len(findings) != 0 {
		t.Fatalf("benign command flagged: %+v", findings)
	}
	if e.Stats().ToolFindings != 1 {
		t.Fatalf("tool findings counter: %d", e.Stats().ToolFindings)
	}
}

func TestToolGuardDisabled(t *testing.T) {
	cfg := testConfig(t)
	e := newTestEngine(t, cfg) // no ToolGuard configured
	if findings := e.CheckToolArgs(`rm -rf /`); len(findings) != 0 {
		t.Fatalf("disabled tool guard flagged: %+v", findings)
	}
}

func TestReconfigureHotReloadsRules(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomWords = map[string][]string{"ALPHA": {"alpha-project"}}
	e := newTestEngine(t, cfg)
	out := mustMask(t, e, `代号 alpha-project`)
	if !strings.Contains(out, "{{ALPHA_") {
		t.Fatalf("initial custom rule missing: %q", out)
	}
	// Hot-reload: drop ALPHA, add BETA.
	next := cfg
	next.CustomWords = map[string][]string{"BETA": {"beta-project"}}
	if err := e.Reconfigure(next); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	out = mustMask(t, e, `代号 alpha-project 与 beta-project`)
	if strings.Contains(out, "{{ALPHA_") || !strings.Contains(out, "{{BETA_") {
		t.Fatalf("reconfigured rules not applied: %q", out)
	}
	if err := e.Reconfigure(Config{Enabled: true, StoreFile: cfg.StoreFile, StoreKeyEnv: cfg.StoreKeyEnv, CustomWords: map[string][]string{"BAD": {"re:["}}}); err == nil {
		t.Fatal("reconfigure must reject invalid regex")
	}
}

func TestRestoreJSONEscapesValues(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomWords = map[string][]string{"QUOTED": {`内"容"`}}
	e := newTestEngine(t, cfg)
	masked := mustMask(t, e, `标记 内"容"`)
	token := masked[strings.Index(masked, "{{") : strings.Index(masked, "}}")+2]
	// The placeholder inside a JSON string must be restored with escaped quotes.
	out, err := e.RestoreJSON(context.Background(), []byte(`{"tag":"`+token+`"}`))
	if err != nil {
		t.Fatalf("RestoreJSON: %v", err)
	}
	if string(out) != `{"tag":"内\"容\""}` {
		t.Fatalf("RestoreJSON escaping wrong: %s", out)
	}
	// Plain Restore keeps the raw original (for non-JSON text contexts).
	plain, err := e.Restore(context.Background(), []byte(`前缀`+token+`后缀`))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if string(plain) != `前缀内"容"后缀` {
		t.Fatalf("plain restore wrong: %s", plain)
	}
}

func TestMaskCountReportsSpans(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	body := []byte(`邮箱 user@example.com 和 13800138000`)
	out, count, err := e.MaskCount(context.Background(), body)
	if err != nil {
		t.Fatalf("MaskCount: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 spans, got %d (%s)", count, out)
	}
	if strings.Contains(string(out), "user@example.com") || strings.Contains(string(out), "13800138000") {
		t.Fatalf("originals leaked: %s", out)
	}
	stats := e.Stats()
	if stats.Masked != 2 || stats.Entities["EMAIL"] != 1 || stats.Entities["PHONE"] != 1 {
		t.Fatalf("stats wrong: %+v", stats)
	}
}

func TestMaskStatsCountOnlyAppliedSpans(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	// URL_QUERY_SECRET and API_KEY match the same value; overlap resolution
	// keeps the earlier (URL_QUERY_SECRET) span, so exactly one replacement
	// happens even though two rules matched.
	body := `https://x.com/a?access_token=sk_live_abcdefghijklmnopqrstuv`
	out, applied, err := e.MaskCount(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("MaskCount: %v", err)
	}
	if applied != 1 {
		t.Fatalf("expected 1 applied span, got %d (%s)", applied, out)
	}
	stats := e.Stats()
	if stats.Masked != 1 {
		t.Fatalf("stats.Masked must count applied spans only, got %d", stats.Masked)
	}
	if !strings.Contains(string(out), "{{URL_QUERY_SECRET_") {
		t.Fatalf("expected URL_QUERY_SECRET placeholder: %s", out)
	}
}
