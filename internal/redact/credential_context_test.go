package redact

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPrivateIPAndWhitespacePasswordMaskAndRestore(t *testing.T) {
	// Use a synthetic password with the reported shape, not the user's value.
	const input = "192.168.3.88  密码 Demo@2402"
	for _, jsonBody := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[jsonBody], func(t *testing.T) {
			e := newTestEngine(t, testConfig(t))
			body := []byte(input)
			if jsonBody {
				body, _ = json.Marshal(map[string]string{"content": input})
			}
			out, n, err := e.MaskCount(context.Background(), body)
			if err != nil || n != 2 {
				t.Fatalf("expected both IP and password masked; count=%d err=%v", n, err)
			}
			if strings.Contains(string(out), "192.168.3.88") || strings.Contains(string(out), "Demo@2402") || !strings.Contains(string(out), "{{IP_PRIVATE_") || !strings.Contains(string(out), "{{SECRET_") {
				t.Fatalf("missing masking: %s", out)
			}
			var restored []byte
			if jsonBody {
				restored, err = e.RestoreJSON(context.Background(), out)
			} else {
				restored, err = e.Restore(context.Background(), out)
			}
			if err != nil || string(restored) != string(body) {
				t.Fatalf("roundtrip changed content: %s %v", restored, err)
			}
		})
	}
}

func TestCredentialContextSeparators(t *testing.T) {
	cases := []string{
		"密码 Demo@2402", "密码  Demo@2402", "密码\tDemo@2402", "密码\nDemo@2402", "密码　Demo@2402",
		"密码为Demo@2402", "密码是 Demo@2402", "密码：Demo@2402", "密码 = Demo@2402",
		"password Demo@2402", "Password Demo@2402", "PASSWORD: Demo@2402", "password is Demo@2402",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			e := newTestEngine(t, testConfig(t))
			out := mustMask(t, e, input)
			if strings.Contains(out, "Demo@2402") || !strings.Contains(out, "{{SECRET_") {
				t.Fatalf("password context not masked: %s", out)
			}
			token := placeholderRE.FindString(out)
			if out != strings.Replace(input, "Demo@2402", token, 1) {
				t.Fatalf("context changed: %s", out)
			}
			restored, err := e.Restore(context.Background(), []byte(out))
			if err != nil || string(restored) != input {
				t.Fatalf("roundtrip failed: %s %v", restored, err)
			}
		})
	}
}

func TestWhitespaceCredentialRuleSkipsExplanatoryText(t *testing.T) {
	for _, input := range []string{"password should be strong", "password requires 12 characters", "token budget is 2402", "密码可以自行设置", "这是密码说明文档"} {
		e := newTestEngine(t, testConfig(t))
		if out := mustMask(t, e, input); out != input {
			t.Fatalf("non-credential changed: %q -> %q", input, out)
		}
	}
}
