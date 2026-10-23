// Package observe inspects upstream response bytes without altering them:
// protocol usage accounting, stream timing (TTFT/TPS), tool-call evidence,
// generated-title collection and SSE streaming with redaction restore.
//
// The gateway treats observe as the read-only "response observation" layer:
// every byte the client receives is written exactly as the upstream produced
// it (or, with redaction enabled, the restored equivalent), and the observer
// only accumulates bounded diagnostics alongside the stream.
package observe

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"safuno/internal/pool"
	"safuno/internal/sessions"
)

const observationLimit = 4 << 20

// Diagnostic identifies one failure on the request/response path.
type Diagnostic struct {
	Layer, Reason, Type, Code, Message string
}

// ProtocolError extracts a safe diagnostic from an upstream error body. Only
// protocol error identifiers are retained; arbitrary error messages may echo
// prompts or credentials, so messages are limited to known safe summaries.
func ProtocolError(body []byte) Diagnostic {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return Diagnostic{}
	}
	raw := root["error"]
	if len(raw) == 0 || string(raw) == "null" {
		var response map[string]json.RawMessage
		if json.Unmarshal(root["response"], &response) == nil {
			raw = response["error"]
		}
	}
	var data struct {
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return Diagnostic{}
	}
	d := Diagnostic{Type: data.Type}
	if len(data.Code) > 0 && string(data.Code) != "null" {
		if json.Unmarshal(data.Code, &d.Code) != nil {
			var number json.Number
			if json.Unmarshal(data.Code, &number) == nil {
				d.Code = number.String()
			}
		}
	}
	if data.Type == "permission_denied_error" && data.Message == "model is not available in the current token plan" {
		d.Reason = "model_not_in_token_plan"
		d.Message = data.Message
	} else if data.Type == "rate_limit_error" && data.Message == "inference exceeds tpm/rpm limit" {
		d.Reason = "model_rate_limit"
		d.Message = data.Message
	}
	return d
}

// Observer inspects a bounded copy; the original response bytes are never changed.
type Observer struct {
	title                                     *TitleObservation
	toolCallIDs                               []string
	now                                       func() time.Time
	observedAt, firstGenerated, lastGenerated time.Time
	responseID                                string
	reported                                  []string
	sse                                       bool
	buf                                       []byte
	event                                     []byte
	dropping                                  bool
	droppingEvent                             bool
	tooLarge                                  bool
	observationGap                            bool
	usage                                     pool.Usage
	failed                                    bool
	terminal                                  bool
	error                                     Diagnostic
}

// NewObserver returns an observer for one upstream response. withTitle attaches
// the generated-title collector when the request was classified as a title
// request (the collector reuses the same bounded byte stream).
func NewObserver(sse, withTitle bool) *Observer {
	o := &Observer{sse: sse}
	if withTitle {
		o.title = &TitleObservation{}
	}
	return o
}

func (o *Observer) Write(b []byte) (int, error) {
	n := len(b)
	if o.now != nil {
		o.observedAt = o.now()
	} else {
		o.observedAt = time.Now()
	}
	if !o.sse {
		if len(o.buf)+n <= observationLimit && !o.tooLarge {
			o.buf = append(o.buf, b...)
		} else {
			o.tooLarge = true
			o.buf = nil
		}
		return n, nil
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		var part []byte
		if i < 0 {
			part = b
			b = nil
		} else {
			part = b[:i+1]
			b = b[i+1:]
		}
		if !o.dropping && len(o.buf)+len(part) <= observationLimit {
			o.buf = append(o.buf, part...)
		} else {
			o.dropping = true
			o.droppingEvent = true
			o.observationGap = true
			o.buf = nil
		}
		if i < 0 {
			continue
		}
		if !o.dropping {
			o.line(o.buf)
		}
		o.buf = nil
		o.dropping = false
	}
	return n, nil
}
func (o *Observer) line(b []byte) {
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		if !o.droppingEvent {
			o.process(o.event)
		}
		o.event = nil
		o.droppingEvent = false
		return
	}
	if o.droppingEvent {
		return
	}
	if strings.HasPrefix(s, "data:") {
		part := strings.TrimPrefix(s, "data:")
		part = strings.TrimPrefix(part, " ")
		if len(o.event)+len(part)+1 > observationLimit {
			o.observationGap = true
			o.droppingEvent = true
			o.event = nil
			return
		}
		if len(o.event) > 0 {
			o.event = append(o.event, '\n')
		}
		o.event = append(o.event, part...)
	}
}
func (o *Observer) Done() {
	if o.sse {
		// Only a blank-line-delimited event is complete. EOF must not turn a
		// truncated data: [DONE] or terminal JSON event into successful delivery.
		return
	} else if !o.tooLarge {
		o.process(o.buf)
	}
}
func number(m map[string]json.RawMessage, key string) (int64, bool) {
	b, ok := m[key]
	if !ok || string(b) == "null" {
		return 0, false
	}
	var n int64
	if json.Unmarshal(b, &n) != nil || n < 0 {
		return 0, false
	}
	return n, true
}
func (o *Observer) process(b []byte) {
	if string(bytes.TrimSpace(b)) == "[DONE]" {
		o.terminal = true
		return
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(b, &root) != nil {
		return
	}
	o.observeToolCalls(b)
	if o.title != nil {
		o.title.observe(b)
	}
	var id string
	_ = json.Unmarshal(root["id"], &id)
	if len(id) <= 256 && id != "" {
		o.responseID = id
	}
	var typ string
	_ = json.Unmarshal(root["type"], &typ)
	if o.sse && generatedDelta(root, typ) {
		o.generated()
	}
	if typ == "error" || typ == "response.failed" || (len(root["error"]) > 0 && string(root["error"]) != "null") {
		o.failed = true
		o.error = ProtocolError(b)
	}
	if typ == "message_stop" || typ == "response.completed" || typ == "response.incomplete" || typ == "response.failed" {
		o.terminal = true
	}
	if typ == "response.incomplete" {
		o.failed = true
		o.error = Diagnostic{Reason: "upstream_response_incomplete", Message: "upstream response ended with incomplete status"}
	}
	uRaw := root["usage"]
	for _, key := range []string{"response", "message"} {
		var nested map[string]json.RawMessage
		if json.Unmarshal(root[key], &nested) == nil {
			if nested["usage"] != nil {
				uRaw = nested["usage"]
			}
			var id string
			_ = json.Unmarshal(nested["id"], &id)
			if id != "" && len(id) <= 256 {
				o.responseID = id
			}
		}
	}
	var u map[string]json.RawMessage
	if json.Unmarshal(uRaw, &u) != nil || u == nil {
		return
	}
	in, hasIn := number(u, "prompt_tokens")
	if !hasIn {
		in, hasIn = number(u, "input_tokens")
	}
	out, hasOut := number(u, "completion_tokens")
	if !hasOut {
		out, hasOut = number(u, "output_tokens")
	}
	total, hasTotal := number(u, "total_tokens")
	if !hasIn && !hasOut && !hasTotal {
		return
	}
	for key, present := range map[string]bool{"input_tokens": hasIn, "output_tokens": hasOut, "total_tokens": hasTotal} {
		if present {
			o.reported = addReported(o.reported, key)
		}
	}
	// Anthropic message_start reports input; message_delta later reports output.
	if hasIn {
		o.usage.Input = in
	}
	if hasOut {
		o.usage.Output = out
	}
	if hasTotal {
		o.usage.Total = total
	} else {
		o.usage.Total = o.usage.Input + o.usage.Output
	}
	cached, hasCached := number(u, "cache_read_input_tokens")
	if !hasCached {
		cached, hasCached = number(u, "prompt_cache_hit_tokens")
	}
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		var details map[string]json.RawMessage
		if json.Unmarshal(u[key], &details) == nil {
			if n, ok := number(details, "cached_tokens"); ok {
				cached = n
				hasCached = true
			}
		}
	}
	if hasCached {
		o.reported = addReported(o.reported, "cached_input_tokens")
		o.usage.Cached = cached
	}
	// Anthropic cache read/creation tokens are separate from input_tokens.
	_, cacheRead := u["cache_read_input_tokens"]
	_, cacheCreate := u["cache_creation_input_tokens"]
	if (cacheRead || cacheCreate) && hasIn {
		creation, _ := number(u, "cache_creation_input_tokens")
		o.usage.Input = in + cached + creation
		if !hasTotal {
			o.usage.Total = o.usage.Input + o.usage.Output
		}
	}
	if reported(o.reported, "input_tokens") && reported(o.reported, "output_tokens") {
		o.reported = addReported(o.reported, "total_tokens")
	}
	o.usage.Known = true
}

func (o *Observer) observeToolCalls(b []byte) {
	var p struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
			Delta struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"content"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"content_block"`
		Output []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"output"`
		Item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"item"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(b, &p) != nil {
		return
	}
	for _, c := range p.Choices {
		for _, call := range c.Message.ToolCalls {
			o.toolCallIDs = AddToolID(o.toolCallIDs, call.ID)
		}
		for _, call := range c.Delta.ToolCalls {
			o.toolCallIDs = AddToolID(o.toolCallIDs, call.ID)
		}
	}
	for _, c := range p.Content {
		if c.Type == "tool_use" {
			o.toolCallIDs = AddToolID(o.toolCallIDs, c.ID)
		}
	}
	if p.ContentBlock.Type == "tool_use" {
		o.toolCallIDs = AddToolID(o.toolCallIDs, p.ContentBlock.ID)
	}
	for _, c := range p.Output {
		if c.Type == "function_call" {
			o.toolCallIDs = AddToolID(o.toolCallIDs, c.CallID)
		}
	}
	if p.Item.Type == "function_call" {
		o.toolCallIDs = AddToolID(o.toolCallIDs, p.Item.CallID)
	}
	// A response envelope is only one level deep in the wire protocol.
	if len(p.Response) > 0 {
		var nested struct {
			Output []struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			} `json:"output"`
		}
		if json.Unmarshal(p.Response, &nested) == nil {
			for _, c := range nested.Output {
				if c.Type == "function_call" {
					o.toolCallIDs = AddToolID(o.toolCallIDs, c.CallID)
				}
			}
		}
	}
}

// AddToolID appends a tool-call id when it is well-formed and not already
// present, bounded to 128 ids so a hostile response cannot exhaust memory.
func AddToolID(ids []string, id string) []string {
	if id != "" && len(id) <= 256 && len(ids) < 128 && !Reported(ids, id) {
		return append(ids, id)
	}
	return ids
}

// Reported reports whether fields contains key.
func Reported(fields []string, key string) bool {
	for _, field := range fields {
		if field == key {
			return true
		}
	}
	return false
}
func reported(fields []string, key string) bool { return Reported(fields, key) }
func addReported(fields []string, key string) []string {
	if !reported(fields, key) {
		return append(fields, key)
	}
	return fields
}

// ---- exported reads for the gateway ----

func (o *Observer) Usage() pool.Usage     { return o.usage }
func (o *Observer) Reported() []string    { return o.reported }
func (o *Observer) ResponseID() string    { return o.responseID }
func (o *Observer) Terminal() bool        { return o.terminal }
func (o *Observer) Failed() bool          { return o.failed }
func (o *Observer) Error() Diagnostic     { return o.error }
func (o *Observer) ToolCallIDs() []string { return o.toolCallIDs }
func (o *Observer) ObservationGap() bool  { return o.observationGap }
func (o *Observer) TooLarge() bool        { return o.tooLarge }
func (o *Observer) IsSSE() bool           { return o.sse }

// TitleResult returns the generated title observation result (text, error).
func (o *Observer) TitleResult() (string, string) {
	if o.title == nil {
		return "", "unobserved_response"
	}
	return o.title.result()
}

// Timing derives TTFT and TPS measurements from the observed stream.
func (o *Observer) Timing(started time.Time, complete bool) *sessions.Timing {
	timing := &sessions.Timing{Streaming: o.sse}
	if !o.sse || o.observationGap || o.firstGenerated.IsZero() {
		return timing
	}
	ttft := float64(o.firstGenerated.Sub(started)) / float64(time.Millisecond)
	if ttft >= 0 {
		timing.TTFTMS = &ttft
	}
	// Events arriving together have no measurable generation interval. Failed or
	// incomplete responses and missing output usage cannot supply an honest TPS.
	span := o.lastGenerated.Sub(o.firstGenerated)
	if complete && !o.failed && o.terminal && span >= time.Millisecond && reported(o.reported, "output_tokens") && o.usage.Output > 0 {
		ms := float64(span) / float64(time.Millisecond)
		timing.GenerationMS = &ms
		timing.GenerationTokens = o.usage.Output
	}
	return timing
}
