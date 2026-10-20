package redact

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func assertCredentialValueMasked(t *testing.T, e *Engine, input, value string, jsonBody bool) {
	t.Helper()
	body := []byte(input)
	if jsonBody {
		body, _ = json.Marshal(map[string]string{"content": input})
	}
	out, _, err := e.MaskCount(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	masked := string(out)
	if jsonBody {
		var fields map[string]string
		if err := json.Unmarshal(out, &fields); err != nil {
			t.Fatal(err)
		}
		masked = fields["content"]
	}
	tokens := placeholderRE.FindAllString(masked, -1)
	start := strings.LastIndex(input, value)
	if len(tokens) != 1 || start < 0 || masked != input[:start]+tokens[0]+input[start+len(value):] {
		t.Fatalf("value not masked in full or context changed: %q", masked)
	}
	var restored []byte
	if jsonBody {
		restored, err = e.RestoreJSON(context.Background(), out)
	} else {
		restored, err = e.Restore(context.Background(), out)
	}
	if err != nil || string(restored) != string(body) {
		t.Fatalf("credential roundtrip changed bytes: %v", err)
	}
}

func TestCredentialCommonSpellingsAudit(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	labels := []string{
		"password", "PASSWORD", "Password", "passwd", "pwd", "passphrase", "login_password", "dbPassword", "MYSQL_ROOT_PASSWORD",
		"密码", "登录密码", "密碼", "口令", "密钥", "秘钥", "密匙", "密鑰", "金鑰", "私钥", "访问密钥", "接口密钥",
		"secret", "client_secret", "clientSecret", "app-secret", "signing_secret", "AWS_SECRET_ACCESS_KEY",
		"token", "Token", "TOKEN", "access_token", "accessToken", "refresh-token", "refreshToken", "id_token", "auth_token", "session_token", "csrf_token", "令牌", "访问令牌", "刷新令牌",
		"key", "KEY", "api_key", "apiKey", "Api-Key", "API KEY", "access_key", "accessKeyId", "secret key", "privateKey", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "MY_APP_TOKEN", "X-API-Key", "X-Auth-Token", "X-CSRF-Token",
		"密码值", "密钥值", "API密钥", "密码 password", "密钥 Key", "Token 令牌", "Bearer Token", "appKey", "encryption_key",
	}
	for _, label := range labels {
		for _, separator := range []string{": ", " = ", " "} {
			for _, jsonBody := range []bool{false, true} {
				t.Run(label+separator+map[bool]string{false: "text", true: "json"}[jsonBody], func(t *testing.T) {
					assertCredentialValueMasked(t, e, label+separator+"Demo@2402", "Demo@2402", jsonBody)
				})
			}
		}
	}
}

func TestCredentialWholeValueAudit(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	cases := []struct{ input, value string }{
		{"password: a", "a"}, {"password=123", "123"}, {"密码 abcd", "abcd"}, {"key=abcdef", "abcdef"}, {"api_key=abcdefghijklmnop", "abcdefghijklmnop"},
		{"password: Demo.2402/abc+=", "Demo.2402/abc+="}, {`password=Demo\2402`, `Demo\2402`}, {"password: 测试密码甲", "测试密码甲"},
		{`password="correct horse battery staple"`, "correct horse battery staple"}, {`password='Demo;2402&abc'`, "Demo;2402&abc"},
		{`password="Demo\"2402"`, `Demo\"2402`}, {"密码：“测试 密码甲”", "测试 密码甲"}, {"密码：「Demo@2402」", "Demo@2402"},
		{"**密码**：`Demo@2402`", "Demo@2402"}, {"API key is Demo@2402", "Demo@2402"}, {"密码为：Demo@2402", "Demo@2402"},
		{"token -> Demo@2402", "Demo@2402"}, {"password: " + strings.Repeat("aB9/", 80), strings.Repeat("aB9/", 80)},
		{"Authorization: Bearer abc", "abc"}, {`"Authorization": "Token abc123"`, "abc123"}, {"Bearer abc.def/ghi+=", "abc.def/ghi+="},
		{"https://example.test/path?key=Demo%402402&mode=read", "Demo%402402"}, {"https://example.test/path?refreshToken=abcdef&mode=read", "abcdef"},
		{"https://example.test/path?api%5Fkey=abc123&mode=read", "abc123"},
		{"token: Bearer abc123", "abc123"}, {"access_token=Bearer abc123", "abc123"}, {"mysql://root:abc@db.example.test/database", "abc"},
		{`curl -H 'X-API-Key: abc123'`, "abc123"}, {`Authorization: Basic YTpi`, "YTpi"}, {`password: "line1` + "\n" + `line2"`, "line1\nline2"},
		{"Bearer abc%2Fdef", "abc%2Fdef"},
		{"password=Demo&2402;abc,def[ghi]{jkl}(mno)", "Demo&2402;abc,def[ghi]{jkl}(mno)"}, {"password=abc;", "abc;"}, {"password=ab'cd\"ef", "ab'cd\"ef"},
		{"tool --api-key=abc123", "abc123"}, {"--password abc123", "abc123"}, {"tool --access-token abc123", "abc123"},
		{"`--api-key=abc123`", "abc123"},
	}
	for i, tc := range cases {
		for _, jsonBody := range []bool{false, true} {
			t.Run(strconv.Itoa(i)+"/"+map[bool]string{false: "text", true: "json"}[jsonBody], func(t *testing.T) {
				assertCredentialValueMasked(t, e, tc.input, tc.value, jsonBody)
			})
		}
	}
}

func TestCredentialJSONFieldValuesAudit(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	for _, field := range []string{"password", "passwd", "pwd", "passphrase", "dbPassword", "MYSQL_ROOT_PASSWORD", "apiKey", "key", "access_token", "accessToken", "refresh-token", "client_secret", "token_value", "apiKeyValue", "OPENAI_API_KEY", "X-API-Key", "Authorization", "密钥", "登录密码", "密碼", "金鑰", "密码值"} {
		for i, value := range []string{"a", "123", "lettersOnly", "含中文的值", "correct horse battery staple", "line1\nline2\t\"quote\"\\path", strings.Repeat("aB9/", 80)} {
			t.Run(field+"/"+strconv.Itoa(i), func(t *testing.T) {
				body, _ := json.Marshal(map[string]string{field: value})
				out, n, err := e.MaskRequestCount(context.Background(), body)
				if err != nil || n != 1 {
					t.Fatalf("expected complete field masking: n=%d err=%v", n, err)
				}
				var fields map[string]string
				if json.Unmarshal(out, &fields) != nil || !placeholderRE.MatchString(fields[field]) || placeholderRE.FindString(fields[field]) != fields[field] {
					t.Fatalf("credential field was partially masked: %s", out)
				}
				restored, err := e.RestoreJSON(context.Background(), out)
				if err != nil || string(restored) != string(body) {
					t.Fatalf("field roundtrip changed bytes: %v", err)
				}
			})
		}
	}
}

func TestCredentialNonSecretsAudit(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	for _, input := range []string{
		"password should be strong", "password requires 12 characters", "password is required", "token budget is 2402", "token count 123456", "API key authentication", "press key Enter", "key benefit is speed", "primary_key=user_id", "public_key=publicValue", "monkey=abc123", "password_length=123456", "max_tokens=123456", "passwordless=abc123", "密码长度为12", "密码需要修改", "这是密钥说明文档", "Bearer authentication scheme",
	} {
		t.Run(input, func(t *testing.T) {
			if out := mustMask(t, e, input); out != input {
				t.Fatalf("non-secret changed: %q", out)
			}
		})
	}
}

func TestNumericCredentialJSONRoundtrip(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	body := []byte(` { "password": 123, "api_key": 900719925474099312345, "token": -1.234e+10, "pin_note": false, "max_tokens": 123, "text_password": "123" } `)
	out, n, err := e.MaskRequestCount(context.Background(), body)
	if err != nil || n != 4 || !json.Valid(out) {
		t.Fatalf("numeric mask failed: n=%d err=%v out=%s", n, err, out)
	}
	if !strings.Contains(string(out), `"max_tokens": 123`) || !strings.Contains(string(out), `"pin_note": false`) || strings.Contains(string(out), "900719925474099312345") {
		t.Fatalf("numeric masking changed protocol data or leaked a credential: %s", out)
	}
	for _, restore := range []func(context.Context, []byte) ([]byte, error){e.RestoreJSON, e.RestoreToolJSON} {
		restored, err := restore(context.Background(), out)
		if err != nil || string(restored) != string(body) {
			t.Fatalf("number precision/type changed: %s %v", restored, err)
		}
	}
	// A separate string token with the same digits must retain its string type.
	stringBody := []byte(`{"password":"123"}`)
	masked, err := e.Mask(context.Background(), stringBody)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := e.RestoreJSON(context.Background(), masked)
	if err != nil || string(restored) != string(stringBody) {
		t.Fatalf("numeric and string mappings collided: %s %v", restored, err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	fresh := newTestEngine(t, e.cfg)
	restored, err = fresh.RestoreToolJSON(context.Background(), out)
	if err != nil || string(restored) != string(body) {
		t.Fatalf("typed credential did not survive restart: %s %v", restored, err)
	}
	// A number echoed as assistant prose must still be JSON string content.
	numberToken := placeholderRE.FindString(string(out))
	response, _ := json.Marshal(map[string]string{"content": numberToken})
	restored, err = e.RestoreJSON(context.Background(), response)
	if err != nil || string(restored) != `{"content":"123"}` {
		t.Fatalf("numeric credential changed the response envelope: %s %v", restored, err)
	}
}

func TestCredentialRuleDisabledAndLengthLimit(t *testing.T) {
	cfg := testConfig(t)
	cfg.DisabledRules = []string{"SECRET", "TOKEN", "URL_QUERY_SECRET"}
	e := newTestEngine(t, cfg)
	for _, input := range []string{`password=a`, `{"password":123,"api_key":"abc"}`, `Bearer abc`, `https://example.test/?key=abc`} {
		if out := mustMask(t, e, input); out != input {
			t.Fatalf("disabled rule still applied: %s", out)
		}
	}
	cfg.DisabledRules = nil
	if err := e.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`password=a`, `{"password":123}`, `Bearer abc`, `https://example.test/?key=abc`} {
		if out := mustMask(t, e, input); out == input {
			t.Fatal("credential rule did not return after hot reload")
		}
	}
	cfg = testConfig(t)
	cfg.MaxTokenLength = 32
	e = newTestEngine(t, cfg)
	if out, _, err := e.MaskRequestCount(context.Background(), []byte("password="+strings.Repeat("a", 33))); err == nil || len(out) != 0 {
		t.Fatal("oversized credential must fail closed instead of leaking a suffix")
	}
}

func TestCredentialMaskingIdempotent(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	for _, input := range []string{`password=a; token=abc.def/ghi+=; key=xyz`, `{"password":"a","access_token":123}`, `https://example.test/?key=abc&mode=read`} {
		first := mustMask(t, e, input)
		if second := mustMask(t, e, first); second != first {
			t.Fatalf("credential placeholder masked again: %s", second)
		}
	}
}

func TestCredentialContainingPlaceholderMaskedInFull(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	for _, value := range []string{"prefix{{EMAIL_bcdfgh}}suffix", "prefix" + mustMask(t, e, "user@example.com") + "suffix"} {
		body, _ := json.Marshal(map[string]string{"password": value})
		masked := mustMask(t, e, string(body))
		var fields map[string]string
		if json.Unmarshal([]byte(masked), &fields) != nil || fields["password"] != placeholderRE.FindString(fields["password"]) {
			t.Fatalf("credential surrounding a placeholder leaked: %s", masked)
		}
		restored, err := e.RestoreJSON(context.Background(), []byte(masked))
		if err != nil || string(restored) != string(body) {
			t.Fatalf("literal placeholder roundtrip failed: %s %v", restored, err)
		}
	}
}
