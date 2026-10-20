package observe

import (
	"encoding/json"
	"strings"

	"daydayup/internal/sessions"
)

// TitleObservation collects generated title text from the same bounded
// protocol stream while the exact response bytes continue to the client.
// Hidden reasoning never enters a title.
type TitleObservation struct {
	text               string
	overflow, rejected bool
}

func (t *TitleObservation) append(text string) {
	if len(t.text)+len(text) > 8192 {
		t.overflow = true
		return
	}
	t.text += text
}
func (t *TitleObservation) replace(text string) {
	if text != "" {
		t.text = ""
		t.append(text)
	}
}

func responseOutput(raw json.RawMessage) (string, bool) {
	var items []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return "", false
	}
	var out strings.Builder
	for _, item := range items {
		if item.Type == "function_call" {
			return "", true
		}
		if item.Type == "message" {
			out.WriteString(sessions.TextContent(item.Content))
		}
	}
	return out.String(), false
}

func (t *TitleObservation) observe(b []byte) {
	var p struct {
		Type       string          `json:"type"`
		Title      string          `json:"title"`
		OutputText string          `json:"output_text"`
		Output     json.RawMessage `json:"output"`
		Response   json.RawMessage `json:"response"`
		Content    json.RawMessage `json:"content"`
		Choices    []struct {
			Index   int `json:"index"`
			Message struct {
				Content   json.RawMessage   `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			Delta struct {
				Content      json.RawMessage   `json:"content"`
				ToolCalls    []json.RawMessage `json:"tool_calls"`
				FunctionCall json.RawMessage   `json:"function_call"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Delta        json.RawMessage `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_block"`
		StopReason string `json:"stop_reason"`
		Status     string `json:"status"`
	}
	if json.Unmarshal(b, &p) != nil {
		return
	}
	if len(p.Choices) > 1 {
		t.rejected = true
	}
	for _, c := range p.Choices {
		if c.Index != 0 || len(c.Message.ToolCalls) > 0 || len(c.Delta.ToolCalls) > 0 || (len(c.Delta.FunctionCall) > 0 && string(c.Delta.FunctionCall) != "null") {
			t.rejected = true
			continue
		}
		if c.FinishReason != "" && c.FinishReason != "stop" {
			t.rejected = true
		}
		t.replace(sessions.TextContent(c.Message.Content))
		t.append(sessions.TextContent(c.Delta.Content))
	}
	switch p.Type {
	case "response.output_text.delta":
		var delta string
		_ = json.Unmarshal(p.Delta, &delta)
		t.append(delta)
	case "response.incomplete", "response.failed":
		t.rejected = true
	case "content_block_start":
		if p.ContentBlock.Type == "tool_use" {
			t.rejected = true
		}
		if p.ContentBlock.Type == "text" {
			t.append(p.ContentBlock.Text)
		}
	case "content_block_delta":
		var d struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(p.Delta, &d)
		if d.Type == "text_delta" {
			t.append(d.Text)
		}
	case "message_delta":
		var d struct {
			StopReason string `json:"stop_reason"`
		}
		_ = json.Unmarshal(p.Delta, &d)
		if d.StopReason != "" && d.StopReason != "end_turn" && d.StopReason != "stop_sequence" {
			t.rejected = true
		}
	case "message":
		t.replace(sessions.TextContent(p.Content))
		var blocks []struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(p.Content, &blocks)
		for _, block := range blocks {
			if block.Type == "tool_use" {
				t.rejected = true
			}
		}
	}
	if p.StopReason != "" && p.StopReason != "end_turn" && p.StopReason != "stop_sequence" {
		t.rejected = true
	}
	if p.Status == "incomplete" || p.Status == "failed" {
		t.rejected = true
	}
	if p.Status != "" && p.Status != "completed" && (p.Type == "" || p.Type == "response" || p.Type == "response.completed") {
		t.rejected = true
	}
	if p.Title != "" {
		raw, _ := json.Marshal(map[string]string{"title": p.Title})
		t.replace(string(raw))
	}
	text, tools := responseOutput(p.Output)
	if tools {
		t.rejected = true
	}
	t.replace(text)
	t.replace(p.OutputText)
	if len(p.Response) > 0 {
		var nested struct {
			Output json.RawMessage `json:"output"`
			Status string          `json:"status"`
		}
		_ = json.Unmarshal(p.Response, &nested)
		text, tools := responseOutput(nested.Output)
		if tools || nested.Status == "incomplete" || nested.Status == "failed" || (nested.Status != "" && nested.Status != "completed" && (p.Type == "" || p.Type == "response.completed")) {
			t.rejected = true
		}
		t.replace(text)
	}
}

func (t *TitleObservation) result() (string, string) {
	if t == nil {
		return "", "unobserved_response"
	}
	if t.overflow {
		return "", "title_observation_limit"
	}
	if t.rejected {
		return "", "title_not_finished_or_contains_tools"
	}
	text, ok := sessions.NormalizeTitle(t.text)
	if !ok {
		return "", "invalid_title"
	}
	return text, ""
}
