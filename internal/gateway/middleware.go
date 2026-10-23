package gateway

// M4 middleware layer (New API semantics): request-side model aliasing
// (model_map) and system-prompt injection, plus response-side reasoning
// content stripping (think_tags). The rewrite runs on the Chat-shaped body
// (after bridge conversion, before the provider), and the response
// post-processor sits between the proxy and the protocol bridge, so the model
// alias and the strip apply to every client protocol.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// middlewareModelFor returns the upstream model for a client model id.
func (s *Server) middlewareModelFor(clientModel string) string {
	if s.Config.Middleware.ModelMap == nil {
		return clientModel
	}
	if upstream, ok := s.Config.Middleware.ModelMap[clientModel]; ok && upstream != "" {
		return upstream
	}
	return clientModel
}

// middlewareClientFor performs the reverse lookup used for response renaming:
// the upstream model answered, but the client should see its own alias. When
// several aliases map to the same upstream, the lexicographically first alias
// wins so the rename is deterministic across requests.
func (s *Server) middlewareClientFor(upstreamModel string) string {
	best := ""
	for client, upstream := range s.Config.Middleware.ModelMap {
		if upstream == upstreamModel && (best == "" || client < best) {
			best = client
		}
	}
	if best == "" {
		return upstreamModel
	}
	return best
}

// middlewareShouldRename reports whether response model renaming is active.
func (s *Server) middlewareShouldRename() bool {
	return len(s.Config.Middleware.ModelMap) > 0
}

// middlewareShouldStripReasoning reports think_tags.strip.
func (s *Server) middlewareShouldStripReasoning() bool {
	return s.Config.Middleware.ThinkTags != nil && s.Config.Middleware.ThinkTags.Strip
}

// applyMiddlewareBody rewrites one request body (Chat shape): the model field
// via model_map and an injected system prompt when a rule matches the client
// model and session identity.
func (s *Server) applyMiddlewareBody(trace *requestTrace, body []byte) []byte {
	mw := s.Config.Middleware
	if len(mw.ModelMap) == 0 && len(mw.SystemPrompt) == 0 {
		return body
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	model := ""
	if raw, ok := root["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	changed := false
	if model != "" && mw.ModelMap != nil {
		if upstream := mw.ModelMap[model]; upstream != "" {
			b, _ := json.Marshal(upstream)
			root["model"] = b
			changed = true
		}
	}
	if len(mw.SystemPrompt) > 0 {
		var inject string
		for _, rule := range mw.SystemPrompt {
			if rule.MatchModel != "" && rule.MatchModel != model {
				continue
			}
			if rule.MatchAgent != "" && !agentMatches(rule.MatchAgent, trace) {
				continue
			}
			inject = rule.Text
			break
		}
		if inject != "" && injectSystemPrompt(root, inject) {
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

// agentMatches reports whether the comma-separated match list contains the
// request's session identity client (case-insensitive).
func agentMatches(match string, trace *requestTrace) bool {
	client := ""
	if trace != nil {
		client = trace.Identity.Client
	}
	if client == "" {
		return false
	}
	for _, part := range strings.Split(match, ",") {
		if strings.EqualFold(strings.TrimSpace(part), client) {
			return true
		}
	}
	return false
}

// injectSystemPrompt prepends a system message when the messages array exists
// and no existing system message already carries the same text.
func injectSystemPrompt(root map[string]json.RawMessage, text string) bool {
	raw, ok := root["messages"]
	if !ok {
		return false
	}
	var messages []map[string]any
	if json.Unmarshal(raw, &messages) != nil {
		return false
	}
	for _, m := range messages {
		if role, _ := m["role"].(string); role == "system" {
			if content, _ := m["content"].(string); strings.Contains(content, text) {
				return false // already present (idempotent)
			}
		}
	}
	msg := map[string]any{"role": "system", "content": text}
	messages = append([]map[string]any{msg}, messages...)
	out, err := json.Marshal(messages)
	if err != nil {
		return false
	}
	root["messages"] = out
	return true
}

// chatRewriteWriter post-processes a Chat response: renames the model field
// back to the client alias and optionally strips reasoning content. It emits
// the same Chat protocol, so it composes with the protocol bridge below it.
type chatRewriteWriter struct {
	http.ResponseWriter
	stream    bool
	strip     bool
	renamer   func(string) string
	pump      ssePump
	buf       *bytes.Buffer
	status    int
	raw       bool // upstream ignored stream:true; pass through
}

// setRaw implements rawSwitcher for the proxy's non-SSE detection. It
// cascades to the inner writer (the protocol bridge or the logged writer) so
// the whole chain passes the upstream's non-SSE body through untouched.
// Unwrap exposes the wrapped writer so layered writers can be searched for the
// gateway's trace holder.
func (c *chatRewriteWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *chatRewriteWriter) setRaw() {
	c.raw = true
	if inner, ok := c.ResponseWriter.(rawSwitcher); ok {
		inner.setRaw()
	}
}

// wantsStream reports streaming mode.
func (c *chatRewriteWriter) wantsStream() bool { return c.stream }

func newChatRewriteWriter(w http.ResponseWriter, stream, strip bool, renamer func(string) string) *chatRewriteWriter {
	c := &chatRewriteWriter{ResponseWriter: w, stream: stream, strip: strip, renamer: renamer}
	if !stream {
		c.buf = &bytes.Buffer{}
	}
	return c
}

func (c *chatRewriteWriter) WriteHeader(status int) {
	c.status = status
	if c.stream || status >= 400 {
		c.ResponseWriter.WriteHeader(status)
	}
}

func (c *chatRewriteWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.status >= 400 || c.raw {
		return c.ResponseWriter.Write(p)
	}
	if c.stream {
		if err := c.streamWrite(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// Flush forwards flushing for the streaming path.
func (c *chatRewriteWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *chatRewriteWriter) streamWrite(p []byte) error {
	c.pump.feedRaw(p, func(data string) {
		trimmed := strings.TrimSpace(data)
		if trimmed == "[DONE]" {
			_ = sse(c.ResponseWriter, "", "[DONE]")
			return
		}
		chunk, err := decodeChatChunk(trimmed)
		if err != nil {
			// Not a Chat event (provider extension): pass the line through.
			_ = sse(c.ResponseWriter, "", data)
			return
		}
		// Events without choices or usage (e.g. an upstream {"error":{...}}
		// event, keep-alives, extension payloads) must reach the client
		// unmodified: the rewriter cannot rebuild what it does not parse.
		if chunk.Choices == nil && chunk.Usage == nil {
			_ = sse(c.ResponseWriter, "", data)
			return
		}
		_ = sse(c.ResponseWriter, "", c.rewriteChunk(chunk))
	})
	return nil
}

// rewriteChunk rebuilds one Chat SSE event with the model renamed and any
// reasoning delta removed. Usage and choices may arrive in the SAME event
// (some providers bundle the final content + finish_reason with usage when
// include_usage is on), so both are rebuilt together — never returned early.
func (c *chatRewriteWriter) rewriteChunk(chunk *chatChunk) string {
	event := map[string]any{}
	if chunk.ID != "" {
		event["id"] = chunk.ID
	}
	model := chunk.Model
	if c.renamer != nil {
		model = c.renamer(model)
	}
	if model != "" {
		event["model"] = model
	}
	if chunk.Usage != nil {
		var usage any
		_ = json.Unmarshal(*chunk.Usage, &usage)
		event["usage"] = usage
	}
	if chunk.Choices == nil {
		return mustJSON(event)
	}
	choices := make([]any, 0, len(chunk.Choices))
	for _, ch := range chunk.Choices {
		delta := map[string]any{}
		if ch.Delta.Role != "" {
			delta["role"] = ch.Delta.Role
		}
		if c.strip && ch.Delta.ReasoningContent != "" {
			// drop reasoning deltas entirely
		} else if ch.Delta.ReasoningContent != "" {
			delta["reasoning_content"] = ch.Delta.ReasoningContent
		}
		if ch.Delta.Content != "" {
			delta["content"] = ch.Delta.Content
		}
		if len(ch.Delta.ToolCalls) > 0 {
			tools := make([]any, 0, len(ch.Delta.ToolCalls))
			for _, tc := range ch.Delta.ToolCalls {
				fn := map[string]any{}
				if tc.Function.Name != "" {
					fn["name"] = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					fn["arguments"] = tc.Function.Arguments
				}
				t := map[string]any{"index": tc.Index, "function": fn}
				if tc.ID != "" {
					t["id"] = tc.ID
				}
				tools = append(tools, t)
			}
			delta["tool_calls"] = tools
		}
		choice := map[string]any{"index": ch.Index, "delta": delta}
		if ch.FinishReason != "" {
			choice["finish_reason"] = ch.FinishReason
		}
		choices = append(choices, choice)
	}
	event["choices"] = choices
	return mustJSON(event)
}

// finalize completes a non-streaming rewrite: parse the Chat JSON, rename the
// model, strip reasoning_content, and forward.
func (c *chatRewriteWriter) finalize() error {
	if c.stream || c.buf == nil {
		return nil
	}
	body := c.buf.Bytes()
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.status >= 400 || c.raw || len(bytes.TrimSpace(body)) == 0 {
		c.ResponseWriter.WriteHeader(c.status)
		_, err := c.ResponseWriter.Write(body)
		return err
	}
	rewritten, err := c.rewriteNonStreaming(body)
	if err != nil {
		// Unparsable: forward the original bytes rather than hanging.
		c.ResponseWriter.WriteHeader(c.status)
		_, writeErr := c.ResponseWriter.Write(body)
		return writeErr
	}
	c.ResponseWriter.WriteHeader(c.status)
	_, err = c.ResponseWriter.Write(rewritten)
	return err
}

func (c *chatRewriteWriter) rewriteNonStreaming(body []byte) ([]byte, error) {
	// UseNumber keeps integer fields (usage token counts etc.) as exact
	// literals — a plain map[string]any round trip would coerce them to
	// float64 and corrupt values beyond 2^53. Field order is not preserved by
	// the map round trip; Chat clients key on field names, not order.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	if c.renamer != nil {
		if model, ok := root["model"].(string); ok {
			root["model"] = c.renamer(model)
		}
	}
	if c.strip {
		if choices, ok := root["choices"].([]any); ok {
			for _, ch := range choices {
				if m, ok := ch.(map[string]any); ok {
					if msg, ok := m["message"].(map[string]any); ok {
						delete(msg, "reasoning_content")
					}
					delete(m, "reasoning_content")
				}
			}
		}
	}
	return json.Marshal(root)
}

// middlewareResponseActive reports whether the Chat response post-processor is
// needed.
func (s *Server) middlewareResponseActive() bool {
	return s.middlewareShouldRename() || s.middlewareShouldStripReasoning() || len(s.Config.ModelRename) > 0
}

