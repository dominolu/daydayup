package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"daydayup/internal/redact"
)

func restoreSSE(t *testing.T, engine *sseChannelRestorer, events []string) (string, error) {
	t.Helper()
	w := httptest.NewRecorder()
	o := NewObserver(true, false)
	err := CopySSERestored(context.Background(), w, strings.NewReader(strings.Join(events, "")), o, SSERestoreHooks{Engine: engine.e})
	return w.Body.String(), err
}

func payloads(t *testing.T, out string) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		s := strings.TrimSpace(line[5:])
		if s == "[DONE]" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("invalid restored SSE JSON: %v (%s)", err, line)
		}
		result = append(result, m)
	}
	return result
}

func TestSSEAllTokenSplitBoundaries(t *testing.T) {
	e := sseRestoreEngine(t)
	token := maskValue(t, e, "EMAIL", "user@example.com")
	for split := 0; split <= len(token); split++ {
		events := []string{fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", token[:split]), fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", token[split:]), "data: [DONE]\n\n"}
		out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), events)
		if err != nil {
			t.Fatal(err)
		}
		var text string
		for _, m := range payloads(t, out) {
			choices := m["choices"].([]any)
			delta := choices[0].(map[string]any)["delta"].(map[string]any)
			text += delta["content"].(string)
		}
		if text != "user@example.com" {
			t.Fatalf("split %d: %q", split, text)
		}
	}
}

func TestSSEInterleavedChoicesAndToolCalls(t *testing.T) {
	for _, tools := range []bool{false, true} {
		t.Run(fmt.Sprint(tools), func(t *testing.T) {
			e := sseRestoreEngine(t)
			a := maskValue(t, e, "EMAIL", "first@example.com")
			b := maskValue(t, e, "EMAIL", "second@example.com")
			var events []string
			for _, chunk := range []struct {
				index int
				text  string
			}{{0, a[:1]}, {1, b[:8]}, {0, a[1:]}, {1, b[8:]}} {
				delta := map[string]any{"content": chunk.text}
				choice := 0
				if tools {
					delta = map[string]any{"tool_calls": []any{map[string]any{"index": chunk.index, "function": map[string]any{"arguments": chunk.text}}}}
				} else {
					choice = chunk.index
				}
				data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": choice, "delta": delta}}})
				events = append(events, "data: "+string(data)+"\n\n")
			}
			events = append(events, "data: [DONE]\n\n")
			out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), events)
			if err != nil {
				t.Fatal(err)
			}
			combined := map[string]string{}
			for _, m := range payloads(t, out) {
				c := m["choices"].([]any)[0].(map[string]any)
				key := fmt.Sprint(c["index"])
				d := c["delta"].(map[string]any)
				if tools {
					tool := d["tool_calls"].([]any)[0].(map[string]any)
					key = fmt.Sprint(tool["index"])
					combined[key] += tool["function"].(map[string]any)["arguments"].(string)
				} else {
					combined[key] += d["content"].(string)
				}
			}
			if combined["0"] != "first@example.com" || combined["1"] != "second@example.com" {
				t.Fatalf("channels mixed: %#v", combined)
			}
		})
	}
}

func TestSSEResponsesArgumentDeltaEscapesAndSeparatesItems(t *testing.T) {
	e := sseRestoreEngine(t)
	// Create a mapping whose original needs another layer of JSON escaping.
	// The email itself contains no quote; surrounding JSON syntax must survive.
	token := maskValue(t, e, "EMAIL", "user@example.com")
	events := []string{fmt.Sprintf("data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":%q}\n\n", `{"email":"`+token[:1]), fmt.Sprintf("data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"delta\":%q}\n\n", `{"value":"plain"}`), fmt.Sprintf("data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":%q}\n\n", token[1:]+`"}`), "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n"}
	out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), events)
	if err != nil {
		t.Fatal(err)
	}
	var args string
	for _, m := range payloads(t, out) {
		if fmt.Sprint(m["output_index"]) == "0" {
			args += m["delta"].(string)
		}
	}
	if args != `{"email":"user@example.com"}` || !json.Valid([]byte(args)) {
		t.Fatalf("invalid args: %s", args)
	}
}

func TestSSELiteralTailBeforeCompletion(t *testing.T) {
	for _, literal := range []string{"literal {", "literal {{", "literal {{template", "nested {{\"a\": 1} plus end"} {
		e := sseRestoreEngine(t)
		events := []string{fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", literal), "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", "data: [DONE]\n\n"}
		out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), events)
		if err != nil {
			t.Fatal(err)
		}
		var text string
		for _, m := range payloads(t, out) {
			for _, c := range m["choices"].([]any) {
				d, _ := c.(map[string]any)["delta"].(map[string]any)
				if s, ok := d["content"].(string); ok {
					text += s
				}
			}
		}
		if text != literal {
			t.Fatalf("literal tail lost: %q -> %q", literal, text)
		}
	}
}

func TestSSEUnknownToolTokenNeverCompletes(t *testing.T) {
	for _, token := range []string{"{{EMAIL_zzzzzz}}", "{{EMAIL_bcd"} {
		e := sseRestoreEngine(t)
		events := []string{fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":%q}}]}}]}\n\n", token), "data: [DONE]\n\n"}
		out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), events)
		if err == nil || strings.Contains(out, "[DONE]") || strings.Contains(out, token) {
			t.Fatalf("unresolved tool call looked successful: %s, %v", out, err)
		}
	}
}

func TestSSEMultilineCRLFAndExactNumbers(t *testing.T) {
	e := sseRestoreEngine(t)
	token := maskValue(t, e, "EMAIL", "user@example.com")
	events := []string{"event: chunk\r\nid: 2\r\ndata: {\"choices\": [\r\ndata: {\"index\":0,\"delta\":{\"content\":\"" + token + "\"}}],\r\ndata: \"large\":9007199254740993}\r\n\r\n", "data: [DONE]\r\n\r\n"}
	out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), events)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "user@example.com") || !strings.Contains(out, "9007199254740993") || strings.Contains(out, "\n\r") && !strings.Contains(out, "\r\n\r\n") {
		t.Fatalf("framing or number corrupted: %q", out)
	}
	if !strings.Contains(out, "event: chunk\r\nid: 2\r\n") {
		t.Fatalf("SSE headers lost: %q", out)
	}
	payloads(t, out)
}

func TestSSERestorationDetectsShortWrite(t *testing.T) {
	e := sseRestoreEngine(t)
	w := &shortRestoreWriter{ResponseRecorder: httptest.NewRecorder()}
	err := CopySSERestored(context.Background(), w, strings.NewReader("data: [DONE]\n\n"), NewObserver(true, false), SSERestoreHooks{Engine: e})
	if err != io.ErrShortWrite {
		t.Fatalf("short write accepted: %v", err)
	}
}

type shortRestoreWriter struct{ *httptest.ResponseRecorder }

func (w *shortRestoreWriter) Write(b []byte) (int, error) {
	return w.ResponseRecorder.Write(b[:len(b)/2])
}

func TestSSEAnthropicBlockIsolationAndSnapshots(t *testing.T) {
	e := sseRestoreEngine(t)
	token := maskValue(t, e, "EMAIL", "user@example.com")
	events := []string{
		fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", token[:1]),
		"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"other block {\"}}\n\n",
		fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", token[1:]),
		"data: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"data: {\"type\":\"message_stop\"}\n\n",
	}
	out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), events)
	if err != nil {
		t.Fatal(err)
	}
	combined := map[string]string{}
	for _, m := range payloads(t, out) {
		if m["type"] == "content_block_delta" {
			d := m["delta"].(map[string]any)
			combined[fmt.Sprint(m["index"])] += d["text"].(string)
		}
	}
	if combined["0"] != "user@example.com" || combined["1"] != "other block {" {
		t.Fatalf("Anthropic blocks mixed or truncated: %v", combined)
	}
}

func TestSSEResponseDoneSnapshotIsIndependent(t *testing.T) {
	e := sseRestoreEngine(t)
	token := maskValue(t, e, "EMAIL", "user@example.com")
	events := []string{
		fmt.Sprintf("data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":%q}\n\n", token),
		fmt.Sprintf("data: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"text\":%q}\n\n", token),
		fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"content\":[{\"text\":%q}]}]}}\n\n", token),
	}
	out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "user@example.com") != 3 || strings.Contains(out, token) {
		t.Fatalf("snapshot corrupted: %s", out)
	}
}

func TestSSEResponseToolQuotesAndLimits(t *testing.T) {
	e := sseRestoreEngine(t)
	cfg := redact.Config{Enabled: true, StoreFile: filepath.Join(t.TempDir(), "map.enc"), CustomWords: map[string][]string{"QUOTED": {`a"b\c`}}}
	if err := e.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	token := maskValue(t, e, "QUOTED", `a"b\c`)
	args := `{"note":"` + token + `"}`
	data, _ := json.Marshal(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": args})
	out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), []string{"data: " + string(data) + "\n\n", "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"})
	if err != nil {
		t.Fatal(err)
	}
	combined := ""
	for _, m := range payloads(t, out) {
		if part, ok := m["delta"].(string); ok {
			combined += part
		}
	}
	var actual map[string]string
	if json.Unmarshal([]byte(combined), &actual) != nil || actual["note"] != `a"b\c` {
		t.Fatalf("quoted args corrupted: %s", out)
	}
}

func TestSSERestoreBuffersAreBounded(t *testing.T) {
	for _, kind := range []string{"event", "tool"} {
		t.Run(kind, func(t *testing.T) {
			e, err := redact.New(redact.Config{Enabled: true, StoreFile: filepath.Join(t.TempDir(), "map.enc"), MaxRestoreBuffer: 512})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			var events []string
			if kind == "event" {
				events = []string{"data: " + strings.Repeat("x", 600) + "\n\n"}
			} else {
				for i := 0; i < 2; i++ {
					data, _ := json.Marshal(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": strings.Repeat("a", 300)})
					events = append(events, "data: "+string(data)+"\n\n")
				}
			}
			events = append(events, "data: [DONE]\n\n")
			out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), events)
			if err == nil || strings.Contains(out, "[DONE]") {
				t.Fatalf("buffer limit accepted: %s %v", out, err)
			}
		})
	}
}

func TestSSEToolUnicodeEscapesAcrossEveryBoundary(t *testing.T) {
	e := sseRestoreEngine(t)
	token := maskValue(t, e, "EMAIL", "user@example.com")
	escaped := strings.ReplaceAll(token, "{", `\u007b`)
	args := `{"email":"` + escaped + `"}`
	for split := 0; split <= len(args); split++ {
		var events []string
		for _, part := range []string{args[:split], args[split:]} {
			data, _ := json.Marshal(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": part})
			events = append(events, "data: "+string(data)+"\n\n")
		}
		events = append(events, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
		out, err := restoreSSE(t, newSSEChannelRestorer(context.Background(), e), events)
		if err != nil {
			t.Fatal(err)
		}
		var combined string
		for _, m := range payloads(t, out) {
			if v, ok := m["delta"].(string); ok {
				combined += v
			}
		}
		var got map[string]string
		if json.Unmarshal([]byte(combined), &got) != nil || got["email"] != "user@example.com" {
			t.Fatalf("split %d escaped args not restored: %s", split, combined)
		}
	}
}
