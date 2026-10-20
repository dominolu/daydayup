package gateway

// OpenAI Responses bridge: /v1/responses <-> Chat Completions.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// responsesToChat converts an OpenAI Responses request into a Chat request.
func responsesToChat(body []byte) ([]byte, error) {
	var req struct {
		Model           string          `json:"model"`
		Input           json.RawMessage `json:"input"`
		Instructions    string          `json:"instructions"`
		MaxOutputTokens int             `json:"max_output_tokens"`
		Stream          bool            `json:"stream"`
		Temperature     *float64        `json:"temperature"`
		Tools           []struct {
			Type     string          `json:"type"`
			Name     string          `json:"name"`
			Function json.RawMessage `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("不是合法的 Responses 请求：%w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("Responses 请求缺少 model")
	}
	chat := map[string]any{"model": req.Model}
	if req.MaxOutputTokens > 0 {
		chat["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	messages, err := responsesInput(req.Input)
	if err != nil {
		return nil, err
	}
	if req.Instructions != "" {
		messages = append([]map[string]any{{"role": "system", "content": req.Instructions}}, messages...)
	}
	chat["messages"] = messages
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type == "web_search_preview" {
				continue // the pool has no search tool
			}
			fn := map[string]any{"type": "function", "function": map[string]any{"name": t.Name}}
			if len(t.Function) > 0 {
				var f struct {
					Description string          `json:"description"`
					Parameters  json.RawMessage `json:"parameters"`
				}
				if json.Unmarshal(t.Function, &f) == nil {
					fn["function"].(map[string]any)["description"] = f.Description
					if len(f.Parameters) > 0 {
						fn["function"].(map[string]any)["parameters"] = f.Parameters
					}
				}
			}
			tools = append(tools, fn)
		}
		chat["tools"] = tools
	}
	if req.Stream {
		chat["stream"] = true
		chat["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(chat)
}

// responsesInput renders the Responses input items as Chat messages.
func responsesInput(raw json.RawMessage) ([]map[string]any, error) {
	// Input may be a string or an array of items.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return []map[string]any{}, nil
		}
		return []map[string]any{{"role": "user", "content": s}}, nil
	}
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		CallID  string          `json:"call_id"`
		Name    string          `json:"name"`
		Output  json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("Responses input 格式无法解析：%w", err)
	}
	var out []map[string]any
	for _, it := range items {
		switch it.Type {
		case "message":
			role := it.Role
			if role == "developer" {
				role = "system"
			}
			if role == "" {
				role = "user"
			}
			out = append(out, map[string]any{"role": role, "content": responseItemText(it.Content)})
		case "function_call":
			out = append(out, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": it.CallID, "type": "function", "function": map[string]any{"name": it.Name, "arguments": responseItemText(it.Content)}}}})
		case "function_call_output":
			out = append(out, map[string]any{"role": "tool", "tool_call_id": it.CallID, "content": responseItemText(it.Output)})
		}
	}
	return out, nil
}

func responseItemText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ---- Responses outbound encoder ----

type responsesEncoder struct {
	pump      ssePump
	started   bool
	completed bool
	lastReason string
	responseID string
	model     string
	output    []any // accumulated output items for response.completed
	msgID     string
	fcIDs     map[string]string
	itemIdx   int
	partIdx   map[string]int
	textBuf   strings.Builder
	usage     extractedUsage
}

func newResponsesEncoder() *responsesEncoder {
	return &responsesEncoder{fcIDs: map[string]string{}, partIdx: map[string]int{}}
}

func (e *responsesEncoder) Write(dst io.Writer, chunk []byte) error {
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

func (e *responsesEncoder) ensureStarted(dst io.Writer, c *chatChunk) error {
	if e.started {
		return nil
	}
	e.started = true
	e.responseID = c.ID
	if e.responseID == "" {
		e.responseID = "resp_" + randSuffix()
	}
	e.model = c.Model
	created := map[string]any{
		"id":         e.responseID,
		"object":     "response",
		"created_at": nowUnix(),
		"status":     "in_progress",
		"model":      e.model,
		"output":     []any{},
		"usage":      map[string]any{},
	}
	if err := sse(dst, "response.created", mustJSON(created)); err != nil {
		return err
	}
	return sse(dst, "response.in_progress", `{"type":"response.in_progress","response":`+mustJSON(created)+`}`)
}

func (e *responsesEncoder) event(dst io.Writer, data string) error {
	c, err := decodeChatChunk(data)
	if err != nil {
		return nil
	}
	if c.Choices == nil && c.Usage == nil {
		return nil
	}
	if err := e.ensureStarted(dst, c); err != nil {
		return err
	}
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		if ch.Delta.Content != "" {
			if err := e.ensureMessageItem(dst); err != nil {
				return err
			}
			e.textBuf.WriteString(ch.Delta.Content)
			payload := map[string]any{"type": "response.output_text.delta", "item_id": e.msgID, "output_index": e.itemIdx, "content_index": 0, "delta": ch.Delta.Content}
			if err := sse(dst, "response.output_text.delta", mustJSON(payload)); err != nil {
				return err
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			if err := e.toolDelta(dst, tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments); err != nil {
				return err
			}
		}
		if ch.FinishReason != "" && !e.completed {
			e.lastReason = ch.FinishReason
		}
	}
	if c.Usage != nil {
		e.usage = extractUsage(c.Usage)
		if !e.completed {
			_ = e.finish(dst, "stop")
		}
	}
	return nil
}

func (e *responsesEncoder) ensureMessageItem(dst io.Writer) error {
	if e.msgID != "" {
		return nil
	}
	e.msgID = "msg_" + randSuffix()
	idx := e.itemIdx
	e.itemIdx++
	item := map[string]any{"id": e.msgID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
	if err := sse(dst, "response.output_item.added", mustJSON(map[string]any{"type": "response.output_item.added", "output_index": idx, "item": item})); err != nil {
		return err
	}
	part := map[string]any{"type": "output_text", "text": ""}
	if err := sse(dst, "response.content_part.added", mustJSON(map[string]any{"type": "response.content_part.added", "item_id": e.msgID, "output_index": idx, "content_index": 0, "part": part})); err != nil {
		return err
	}
	e.output = append(e.output, map[string]any{"id": e.msgID, "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": ""}}})
	return nil
}

func (e *responsesEncoder) toolDelta(dst io.Writer, index int, id, name, args string) error {
	fcID, ok := e.fcIDs[keyOf(index)]
	if !ok {
		if id != "" {
			fcID = id
		} else {
			fcID = "fc_" + randSuffix()
		}
		e.fcIDs[keyOf(index)] = fcID
		idx := e.itemIdx
		e.itemIdx++
		item := map[string]any{"id": fcID, "type": "function_call", "status": "in_progress", "call_id": fcID, "name": name, "arguments": ""}
		if err := sse(dst, "response.output_item.added", mustJSON(map[string]any{"type": "response.output_item.added", "output_index": idx, "item": item})); err != nil {
			return err
		}
		e.output = append(e.output, map[string]any{"id": fcID, "type": "function_call", "status": "completed", "call_id": fcID, "name": name, "arguments": ""})
	}
	if args != "" {
		if err := sse(dst, "response.function_call_arguments.delta", mustJSON(map[string]any{"type": "response.function_call_arguments.delta", "item_id": fcID, "output_index": indexOfOutput(e.output, fcID), "delta": args})); err != nil {
			return err
		}
		e.appendArgs(fcID, args)
	}
	return nil
}

func keyOf(index int) string {
	b, _ := json.Marshal(index)
	return string(b)
}

func indexOfOutput(output []any, id string) int {
	for i, v := range output {
		if m, ok := v.(map[string]any); ok && m["id"] == id {
			return i
		}
	}
	return -1
}

func (e *responsesEncoder) appendArgs(id, args string) {
	for i, v := range e.output {
		if m, ok := v.(map[string]any); ok && m["id"] == id {
			if cur, ok := m["arguments"].(string); ok {
				m["arguments"] = cur + args
			}
			_ = i
		}
	}
}

func (e *responsesEncoder) finish(dst io.Writer, reason string) error {
	e.completed = true
	if e.lastReason != "" {
		reason = e.lastReason
	}
	// Close open items.
	if e.msgID != "" {
		idx := indexOfOutput(e.output, e.msgID)
		if idx < 0 {
			return nil
		}
		text := e.textBuf.String()
		if m, ok := e.output[idx].(map[string]any); ok {
			if content, ok := m["content"].([]any); ok && len(content) > 0 {
				if part, ok := content[0].(map[string]any); ok {
					part["text"] = text
				}
			}
		}
		if err := sse(dst, "response.output_text.done", mustJSON(map[string]any{"type": "response.output_text.done", "item_id": e.msgID, "output_index": idx, "content_index": 0, "text": text})); err != nil {
			return err
		}
		if err := sse(dst, "response.content_part.done", mustJSON(map[string]any{"type": "response.content_part.done", "item_id": e.msgID, "output_index": idx, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text}})); err != nil {
			return err
		}
		if err := sse(dst, "response.output_item.done", mustJSON(map[string]any{"type": "response.output_item.done", "output_index": idx, "item": e.output[idx]})); err != nil {
			return err
		}
	}
	for _, v := range e.output {
		if m, ok := v.(map[string]any); ok && m["type"] == "function_call" {
			id, _ := m["id"].(string)
			idx := indexOfOutput(e.output, id)
			if idx < 0 {
				continue
			}
			if err := sse(dst, "response.function_call_arguments.done", mustJSON(map[string]any{"type": "response.function_call_arguments.done", "item_id": id, "output_index": idx, "arguments": m["arguments"]})); err != nil {
				return err
			}
			if err := sse(dst, "response.output_item.done", mustJSON(map[string]any{"type": "response.output_item.done", "output_index": idx, "item": m})); err != nil {
				return err
			}
		}
	}
	full := map[string]any{
		"id":         e.responseID,
		"object":     "response",
		"created_at": nowUnix(),
		"status":     "completed",
		"model":      e.model,
		"output":     e.output,
		"usage":      map[string]any{"input_tokens": e.usage.Input, "output_tokens": e.usage.Output, "total_tokens": e.usage.Input + e.usage.Output},
	}
	if reason != "" {
		full["status_details"] = map[string]any{"reason": reason}
	}
	if err := sse(dst, "response.completed", mustJSON(map[string]any{"type": "response.completed", "response": full})); err != nil {
		return err
	}
	return nil
}

// End emits response.completed when the upstream stream ended without one.
func (e *responsesEncoder) End(dst io.Writer) error {
	if e.completed {
		return nil
	}
	if !e.started {
		return nil // nothing was streamed; nothing to close
	}
	return e.finish(dst, "stop")
}

func (e *responsesEncoder) TranslateNonStreaming(body []byte) ([]byte, error) {
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
	id := resp.ID
	if id == "" {
		id = "resp_" + randSuffix()
	}
	output := []any{}
	for _, ch := range resp.Choices {
		if ch.Message.Content != "" {
			msgID := "msg_" + randSuffix()
			output = append(output, map[string]any{"id": msgID, "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": ch.Message.Content}}})
		}
		for _, tc := range ch.Message.ToolCalls {
			output = append(output, map[string]any{"id": tc.ID, "type": "function_call", "status": "completed", "call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments})
		}
	}
	u := extractUsage(resp.Usage)
	return json.Marshal(map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": nowUnix(),
		"status":     "completed",
		"model":      resp.Model,
		"output":     output,
		"usage":      map[string]any{"input_tokens": u.Input, "output_tokens": u.Output, "total_tokens": u.Input + u.Output},
	})
}

// mustJSON marshals without error (payloads are static shapes).
func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}