package gateway

// Anthropic Messages bridge: /v1/messages <-> Chat Completions.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// messagesToChat converts an Anthropic Messages request into an OpenAI Chat
// request. Text content blocks are joined; non-text blocks (images, documents)
// are dropped because this gateway is text-only. Tool use/tool results are
// converted into OpenAI tool messages.
func messagesToChat(body []byte) ([]byte, error) {
	var req struct {
		Model       string `json:"model"`
		MaxTokens   int    `json:"max_tokens"`
		Stream      bool   `json:"stream"`
		Temperature *float64 `json:"temperature"`
		System      json.RawMessage `json:"system"`
		Messages    []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("不是合法的 Messages 请求：%w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("Messages 请求缺少 model")
	}
	chat := map[string]any{
		"model": req.Model,
	}
	if req.MaxTokens > 0 {
		chat["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	var messages []map[string]any
	if len(req.System) > 0 && string(req.System) != "null" {
		messages = append(messages, map[string]any{"role": "system", "content": systemText(req.System)})
	}
	for _, m := range req.Messages {
		blocks := contentBlocks(m.Content)
		switch m.Role {
		case "assistant":
			// Split text and tool_use blocks: text goes into content, every
			// tool_use becomes an OpenAI function tool_call so the upstream can
			// return matching tool_call_ids on the next turn.
			var text []string
			var calls []map[string]any
			for _, b := range blocks {
				switch b.Type {
				case "text":
					text = append(text, b.Text)
				case "tool_use":
					args := json.RawMessage(`{}`)
					if len(b.Input) > 0 {
						args = b.Input
					}
					argStr, _ := json.Marshal(args)
					calls = append(calls, map[string]any{"id": b.ID, "type": "function", "function": map[string]any{"name": b.Name, "arguments": string(argStr)}})
				}
			}
			msg := map[string]any{"role": "assistant", "content": strings.Join(text, "\n\n")}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			messages = append(messages, msg)
		case "user":
			results := toolResults(blocks)
			if len(results) > 0 {
				// Every tool_result becomes a role:tool message carrying the
				// tool_call_id so the upstream can pair it with the call.
				for _, tr := range results {
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": tr.ID, "content": tr.Text})
				}
				// User text in the same message stays a regular user message.
				if text := blockText(blocks); text != "" {
					messages = append(messages, map[string]any{"role": "user", "content": text})
				}
				continue
			}
			content := blockText(blocks)
			if len(messages) > 0 && messages[len(messages)-1]["role"] == "user" {
				// OpenAI rejects consecutive user turns; merge texts.
				prev := messages[len(messages)-1]["content"].(string)
				messages[len(messages)-1]["content"] = prev + "\n\n" + content
				continue
			}
			messages = append(messages, map[string]any{"role": "user", "content": content})
		default:
			messages = append(messages, map[string]any{"role": m.Role, "content": blockText(blocks)})
		}
	}
	chat["messages"] = messages
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
		chat["tools"] = tools
	}
	if req.Stream {
		chat["stream"] = true
		chat["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(chat)
}

// systemText renders the Anthropic system field (string or block array).
func systemText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// contentBlock is one Anthropic content block.
type contentBlock struct {
	Type      string
	Text      string
	ID        string
	Name      string
	Input     json.RawMessage
	ToolUseID string
	Content   json.RawMessage
}

// contentBlocks parses an Anthropic content field (string or block array).
func contentBlocks(raw json.RawMessage) []contentBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []contentBlock{{Type: "text", Text: s}}
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	out := make([]contentBlock, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, contentBlock{Type: b.Type, Text: b.Text, ID: b.ID, Name: b.Name, Input: b.Input, ToolUseID: b.ToolUseID, Content: b.Content})
	}
	return out
}

// blockText joins the text blocks of a content field.
func blockText(blocks []contentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// toolResults extracts tool_result blocks with their text content.
type toolResult struct {
	ID   string
	Text string
}

func toolResults(blocks []contentBlock) []toolResult {
	var out []toolResult
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		out = append(out, toolResult{ID: b.ToolUseID, Text: contentInnerText(b.Content)})
	}
	return out
}

func contentInnerText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// ---- Anthropic outbound encoder ----

type anthropicEncoder struct {
	pump          ssePump
	sentStart     bool
	sentStop      bool
	stopSent      bool
	messageID     string
	model         string
	index         int // next content block index
	textBlockIdx  int // content block index of the open text block
	textOpen      bool
	hasText       bool
	toolState     map[int]*anthropicTool // openai tool_call index -> state
	openIndices   []int                  // content block indices currently open
}

type anthropicTool struct {
	blockIndex int
	id         string
	name       string
	args       strings.Builder
}

func newAnthropicEncoder() *anthropicEncoder {
	return &anthropicEncoder{toolState: map[int]*anthropicTool{}}
}

func (e *anthropicEncoder) Write(dst io.Writer, chunk []byte) error {
	var lastErr error
	e.pump.feed(chunk, func(data string) {
		if lastErr != nil {
			return
		}
		if err := e.event(dst, data); err != nil {
			lastErr = err
		}
	})
	return lastErr
}

func sse(w io.Writer, event, data string) error {
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return nil
}

func writeJSONValue(dst io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = dst.Write(b)
	return err
}

func (e *anthropicEncoder) start(dst io.Writer, c *chatChunk) error {
	if e.sentStart {
		return nil
	}
	e.sentStart = true
	if e.messageID == "" {
		e.messageID = c.ID
		if e.messageID == "" {
			e.messageID = "msg_" + randSuffix()
		}
	}
	if e.model == "" {
		e.model = c.Model
	}
	return sse(dst, "message_start", `{"type":"message_start","message":{"id":`+jsonString(e.messageID)+`,"type":"message","role":"assistant","model":`+jsonString(e.model)+`,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`)
}

func (e *anthropicEncoder) event(dst io.Writer, data string) error {
	c, err := decodeChatChunk(data)
	if err != nil {
		return nil // non-JSON (e.g. keep-alive comment) — ignore
	}
	if c.Choices == nil && c.Usage == nil {
		return nil
	}
	if err := e.start(dst, c); err != nil {
		return err
	}
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		if ch.Delta.Content != "" {
			if !e.textOpen {
				if err := e.startTextBlock(dst); err != nil {
					return err
				}
			}
			if err := e.delta(dst, map[string]any{"type": "content_block_delta", "index": e.textIndex(), "delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content}}); err != nil {
				return err
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			st := e.toolState[tc.Index]
			if st == nil {
				st = &anthropicTool{blockIndex: -1}
				e.toolState[tc.Index] = st
			}
			if tc.ID != "" && st.id == "" {
				st.id = tc.ID
			}
			if tc.Function.Name != "" && st.name == "" {
				st.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				st.args.WriteString(tc.Function.Arguments)
			}
			if st.blockIndex < 0 {
				// Delay the content_block_start until both id and name are
				// known, so the tool_use block is complete; arguments buffered
				// so far are flushed as the first input_json_delta.
				if st.id == "" || st.name == "" {
					continue
				}
				if err := e.openTool(dst, st); err != nil {
					return err
				}
				if buffered := st.args.String(); buffered != "" {
					st.args.Reset()
					if err := e.toolDelta(dst, st, buffered); err != nil {
						return err
					}
				}
			} else if tc.Function.Arguments != "" {
				st.args.Reset()
				if err := e.toolDelta(dst, st, tc.Function.Arguments); err != nil {
					return err
				}
			}
		}
		if ch.FinishReason != "" && !e.sentStop {
			e.sentStop = true
			e.closeAllBlocks(dst)
			stop := mapFinishReason(ch.FinishReason)
			payload := fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":null},"usage":{"output_tokens":0}}`, jsonString(stop))
			if err := sse(dst, "message_delta", payload); err != nil {
				return err
			}
		}
	}
	if c.Usage != nil {
		u := extractUsage(c.Usage)
		payload := fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`, u.Input, u.Output)
		if err := sse(dst, "message_delta", payload); err != nil {
			return err
		}
		if !e.sentStop {
			e.sentStop = true
			e.closeAllBlocks(dst)
		}
		if err := sse(dst, "message_stop", `{"type":"message_stop"}`); err != nil {
			return err
		}
		e.stopSent = true
	}
	return nil
}

// End emits message_stop when the upstream stream ended without one.
func (e *anthropicEncoder) End(dst io.Writer) error {
	if e.stopSent {
		return nil
	}
	if !e.sentStart {
		return nil
	}
	e.stopSent = true
	e.closeAllBlocks(dst)
	return sse(dst, "message_stop", `{"type":"message_stop"}`)
}

func (e *anthropicEncoder) textIndex() int { return e.textBlockIdx }

// openTool emits content_block_start for one tool_use (id and name known).
func (e *anthropicEncoder) openTool(dst io.Writer, st *anthropicTool) error {
	e.closeTextBlock(dst)
	st.blockIndex = e.index
	e.index++
	e.openIndices = append(e.openIndices, st.blockIndex)
	payload := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, st.blockIndex, jsonString(st.id), jsonString(st.name))
	return sse(dst, "content_block_start", payload)
}

// toolDelta emits one input_json_delta for an open tool block.
func (e *anthropicEncoder) toolDelta(dst io.Writer, st *anthropicTool, partial string) error {
	payload := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, st.blockIndex, jsonString(partial))
	return sse(dst, "content_block_delta", payload)
}

func (e *anthropicEncoder) startTextBlock(dst io.Writer) error {
	e.textBlockIdx = e.index
	e.index++
	e.openIndices = append(e.openIndices, e.textBlockIdx)
	e.hasText = true
	e.textOpen = true
	payload := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, e.textBlockIdx)
	return sse(dst, "content_block_start", payload)
}

func (e *anthropicEncoder) closeTextBlock(dst io.Writer) {
	if !e.textOpen {
		return
	}
	e.textOpen = false
	_ = sse(dst, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, e.textBlockIdx))
	e.removeOpen(e.textBlockIdx)
}

func (e *anthropicEncoder) closeAllBlocks(dst io.Writer) {
	e.closeTextBlock(dst)
	for _, idx := range e.openIndices {
		_ = sse(dst, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
	}
	e.openIndices = nil
}

func (e *anthropicEncoder) removeOpen(idx int) {
	out := e.openIndices[:0]
	for _, i := range e.openIndices {
		if i != idx {
			out = append(out, i)
		}
	}
	e.openIndices = out
}

func (e *anthropicEncoder) delta(dst io.Writer, v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return sse(dst, "content_block_delta", string(b))
}

func (e *anthropicEncoder) TranslateNonStreaming(body []byte) ([]byte, error) {
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("上游返回了无法解析的响应：%w", err)
	}
	msgID := resp.ID
	if msgID == "" {
		msgID = "msg_" + randSuffix()
	}
	content := []any{}
	stopReason := "end_turn"
	for _, ch := range resp.Choices {
		if ch.Message.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": ch.Message.Content})
		}
		for _, tc := range ch.Message.ToolCalls {
			var input any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			if input == nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
		}
		if ch.FinishReason != "" {
			stopReason = mapFinishReason(ch.FinishReason)
		}
	}
	out := map[string]any{
		"id":             msgID,
		"type":           "message",
		"role":           "assistant",
		"model":          resp.Model,
		"content":        content,
		"stop_reason":    stopReason,
		"stop_sequence":  nil,
		"usage":          anthropicUsage(resp.Usage),
	}
	return json.Marshal(out)
}

func anthropicUsage(raw *json.RawMessage) map[string]any {
	u := extractUsage(raw)
	return map[string]any{"input_tokens": u.Input, "output_tokens": u.Output}
}

// extractedUsage is the shared usage view across Chat / Messages / Responses.
type extractedUsage struct {
	Input  int64
	Output int64
	Cached int64
}

func extractUsage(raw *json.RawMessage) extractedUsage {
	var u extractedUsage
	if raw == nil || len(*raw) == 0 || string(*raw) == "null" {
		return u
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(*raw, &m) != nil {
		return u
	}
	if n, ok := numberFrom(m, "prompt_tokens"); ok {
		u.Input = n
	}
	if n, ok := numberFrom(m, "input_tokens"); ok {
		u.Input = n
	}
	if n, ok := numberFrom(m, "completion_tokens"); ok {
		u.Output = n
	}
	if n, ok := numberFrom(m, "output_tokens"); ok {
		u.Output = n
	}
	if n, ok := numberFrom(m, "cache_read_input_tokens"); ok {
		u.Cached = n
	}
	if n, ok := numberFrom(m, "prompt_cache_hit_tokens"); ok {
		u.Cached = n
	}
	return u
}

func numberFrom(m map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := m[key]
	if !ok || string(raw) == "null" {
		return 0, false
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return reason
	}
}