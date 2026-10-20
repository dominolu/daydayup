package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestRedactionPrivateIPAndWhitespacePasswordRoundtrip(t *testing.T) {
	// A synthetic credential reproduces the reported format without retaining it.
	const input = "192.168.3.88  密码 Demo@2402"
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Messages []struct{ Content string } `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 1 {
					t.Error("invalid upstream request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				content := req.Messages[0].Content
				if strings.Contains(content, "192.168.3.88") || strings.Contains(content, "Demo@2402") || !strings.Contains(content, "{{IP_PRIVATE_") || !strings.Contains(content, "{{SECRET_") {
					t.Errorf("upstream did not receive both placeholders: %q", content)
				}
				message := "message"
				w.Header().Set("Content-Type", "application/json")
				if stream {
					message = "delta"
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: ")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, message: map[string]any{"content": content}, "finish_reason": "stop"}}})
				if stream {
					_, _ = io.WriteString(w, "\ndata: [DONE]\n\n")
				}
			})
			s, _ := redactedGateway(t, upstream)
			body := `{"model":"m","messages":[{"role":"user","content":` + strconv.Quote(input) + `}],"stream":` + strconv.FormatBool(stream) + `}`
			rec := call(s, "POST", "/v1/chat/completions", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			out := rec.Body.String()
			if !strings.Contains(out, input) || strings.Contains(out, "{{IP_PRIVATE_") || strings.Contains(out, "{{SECRET_") {
				t.Fatalf("client response was not restored: %s", out)
			}
			if stream && !strings.Contains(out, "data: [DONE]") {
				t.Fatalf("stream terminal event missing: %s", out)
			}
		})
	}
}

func TestRedactionCredentialFormatsAndNumericToolArguments(t *testing.T) {
	const args = `{"password":123,"apiKey":"lettersOnly","access_token":"abc.def/ghi+=","note":"line\nquote\"\\path"}`
	const content = "password=a\nX-API-Key: Demo@2402\n密码：\"测试 密码甲\""
	for _, stream := range []bool{false, true} {
		t.Run(strconv.FormatBool(stream), func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Messages []struct {
						Content   string
						ToolCalls []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 2 || len(req.Messages[1].ToolCalls) != 1 {
					t.Error("invalid upstream JSON")
					w.WriteHeader(400)
					return
				}
				maskedContent := req.Messages[0].Content
				maskedArgs := req.Messages[1].ToolCalls[0].Function.Arguments
				if !strings.HasPrefix(maskedContent, "password={{SECRET_") || strings.Contains(maskedContent, "Demo@2402") || strings.Contains(maskedContent, "测试 密码甲") {
					t.Errorf("credential text leaked upstream: %s", maskedContent)
				}
				var fields map[string]any
				if json.Unmarshal([]byte(maskedArgs), &fields) != nil {
					t.Error("invalid upstream tool arguments")
				}
				for _, field := range []string{"password", "apiKey", "access_token"} {
					value, ok := fields[field].(string)
					if !ok || !strings.HasPrefix(value, "{{SECRET_") || !strings.HasSuffix(value, "}}") {
						t.Errorf("tool credential %s was not completely masked", field)
					}
				}
				message := "message"
				w.Header().Set("Content-Type", "application/json")
				if stream {
					message = "delta"
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: ")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, message: map[string]any{
					"content":    maskedContent,
					"tool_calls": []any{map[string]any{"index": 0, "id": "call_test", "type": "function", "function": map[string]any{"name": "record", "arguments": maskedArgs}}},
				}, "finish_reason": "tool_calls"}}})
				if stream {
					_, _ = io.WriteString(w, "\ndata: [DONE]\n\n")
				}
			})
			s, _ := redactedGateway(t, upstream)
			body, _ := json.Marshal(map[string]any{"model": "m", "stream": stream, "messages": []any{
				map[string]any{"role": "user", "content": content},
				map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_test", "type": "function", "function": map[string]any{"name": "record", "arguments": args}}}},
			}})
			rec := call(s, "POST", "/v1/chat/completions", string(body))
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var events [][]byte
			if stream {
				scanner := bufio.NewScanner(rec.Body)
				for scanner.Scan() {
					line := strings.TrimPrefix(scanner.Text(), "data: ")
					if json.Valid([]byte(line)) {
						events = append(events, []byte(line))
					}
				}
			} else {
				events = append(events, rec.Body.Bytes())
			}
			var restoredContent, restoredArgs string
			for _, event := range events {
				var doc struct {
					Choices []struct {
						Message struct {
							Content   string
							ToolCalls []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
						}
						Delta struct {
							Content   string
							ToolCalls []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
						}
					}
				}
				if err := json.Unmarshal(event, &doc); err != nil {
					t.Fatal(err)
				}
				for _, choice := range doc.Choices {
					restoredContent += choice.Message.Content + choice.Delta.Content
					for _, call := range choice.Message.ToolCalls {
						restoredArgs += call.Function.Arguments
					}
					for _, call := range choice.Delta.ToolCalls {
						restoredArgs += call.Function.Arguments
					}
				}
			}
			if restoredContent != content || restoredArgs != args {
				t.Fatalf("credential content or numeric argument type changed: content=%q args=%s", restoredContent, restoredArgs)
			}
		})
	}
}
