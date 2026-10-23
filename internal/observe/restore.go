package observe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"safuno/internal/redact"
)

type textTemplate struct {
	root any
	path []string
}

type sseChannelRestorer struct {
	e             *redact.Engine
	ctx           context.Context
	text          map[string]*redact.RestoreSession
	tool          map[string]*redact.RestoreSession
	toolAccum     map[string]*bytes.Buffer
	templates     map[string]textTemplate
	toolTemplates map[string]textTemplate
	findings      []redact.ToolFinding
	tails         []byte
	bad           bool
	buffered      int64
}

func newSSEChannelRestorer(ctx context.Context, e *redact.Engine) *sseChannelRestorer {
	return &sseChannelRestorer{e: e, ctx: ctx, text: map[string]*redact.RestoreSession{}, tool: map[string]*redact.RestoreSession{}, toolAccum: map[string]*bytes.Buffer{}, templates: map[string]textTemplate{}, toolTemplates: map[string]textTemplate{}}
}

// feed is also used for a single data line. Production streaming assembles full
// SSE events first, so multiline data fields and CRLF follow the SSE framing.
func (r *sseChannelRestorer) feed(line []byte) ([]byte, error) {
	payload, ok := dataLine(line)
	if !ok || len(payload) == 0 || string(payload) == "[DONE]" {
		return line, nil
	}
	out, err := r.feedPayload(payload)
	if err != nil {
		return nil, err
	}
	ending := ""
	if bytes.HasSuffix(line, []byte("\r\n")) {
		ending = "\r\n"
	} else if bytes.HasSuffix(line, []byte("\n")) {
		ending = "\n"
	}
	return append(append([]byte("data: "), out...), []byte(ending)...), nil
}

func dataLine(line []byte) ([]byte, bool) {
	line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	return bytes.TrimPrefix(line[5:], []byte(" ")), true
}

func (r *sseChannelRestorer) feedPayload(payload []byte) ([]byte, error) {
	if !json.Valid(payload) {
		return payload, nil
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return payload, nil
	}
	m, _ := root.(map[string]any)
	typ, _ := m["type"].(string)
	base := "root"
	if strings.HasPrefix(typ, "response.") {
		base = "r:" + indexOf(m, "output_index", "item_id")
	} else if strings.HasPrefix(typ, "content_block_") {
		base = "a:" + indexOf(m, "index", "")
	}
	// A completion snapshot is independent of the delta channels. Feeding the
	// full snapshot into an incremental parser would duplicate pending prefixes.
	incremental := typ == "" || typ == "content_block_delta" || strings.HasSuffix(typ, ".delta")
	if !incremental {
		if typ == "content_block_stop" || strings.HasSuffix(typ, ".done") {
			if err := r.finishPrefix(base + "/"); err != nil {
				return nil, err
			}
		}
		return r.e.RestoreJSON(r.ctx, payload)
	}
	if err := r.walk(root, root, base, nil, typ); err != nil {
		return nil, err
	}
	if choices, ok := m["choices"].([]any); ok {
		for n, choice := range choices {
			c, _ := choice.(map[string]any)
			if reason, ok := c["finish_reason"].(string); ok && reason != "" {
				idx := channelIndex(c["index"])
				if idx == "" {
					idx = strconv.Itoa(n)
				}
				if err := r.finishPrefix(base + "/c:" + idx + "/"); err != nil {
					return nil, err
				}
			}
		}
	}
	return json.Marshal(root)
}

func (r *sseChannelRestorer) walk(root, v any, base string, path []string, typ string) error {
	switch node := v.(type) {
	case []any:
		for n, item := range node {
			if err := r.walk(root, item, base, appendPath(path, strconv.Itoa(n)), typ); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := node[key]
			nextPath := appendPath(path, key)
			if rows, ok := value.([]any); ok && (key == "choices" || key == "tool_calls") {
				label := "/c:"
				if key == "tool_calls" {
					label = "/t:"
				}
				for n, row := range rows {
					fields, _ := row.(map[string]any)
					index := channelIndex(fields["index"])
					if index == "" {
						index = strconv.Itoa(n)
					}
					if err := r.walk(root, row, base+label+index, appendPath(nextPath, strconv.Itoa(n)), typ); err != nil {
						return err
					}
				}
				continue
			}
			if text, ok := value.(string); ok {
				tool := key == "arguments" || key == "partial_json" || (key == "delta" && strings.Contains(typ, "function_call_arguments"))
				isText := key == "content" || key == "text" || key == "output_text" || key == "reasoning_content" || key == "delta"
				if tool {
					r.toolTemplates[base+"/args"] = textTemplate{root: root, path: nextPath}
					restored, err := r.restoreTool(base+"/args", text)
					if err != nil {
						return err
					}
					node[key] = restored
				} else if isText {
					channel := base + "/" + key
					if fields, ok := root.(map[string]any); ok {
						if index, exists := fields["content_index"]; exists {
							channel += "/" + channelIndex(index)
						}
					}
					// Save the delta shape before changing its payload, for a literal tail
					// that must be emitted before the channel's completion event.
					r.templates[channel] = textTemplate{root: root, path: nextPath}
					restored, err := r.restoreText(channel, text)
					if err != nil {
						return err
					}
					node[key] = restored
				}
			} else if err := r.walk(root, value, base, nextPath, typ); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendPath(path []string, value string) []string {
	out := make([]string, len(path)+1)
	copy(out, path)
	out[len(path)] = value
	return out
}
func indexOf(m map[string]any, key, fallback string) string {
	if v := channelIndex(m[key]); v != "" {
		return v
	}
	if v := channelIndex(m[fallback]); v != "" {
		return v
	}
	return "0"
}
func channelIndex(v any) string {
	switch t := v.(type) {
	case json.Number:
		return t.String()
	case float64:
		return strconv.Itoa(int(t))
	case string:
		return t
	}
	return ""
}

func (r *sseChannelRestorer) restoreText(key, value string) (string, error) {
	session := r.text[key]
	if session == nil {
		if len(r.text)+len(r.tool) >= 1024 {
			return "", fmt.Errorf("too many restoration channels")
		}
		session = r.e.NewRestoreSession()
		r.text[key] = session
	}
	out, err := session.Push(r.ctx, []byte(value))
	return string(out), err
}

func (r *sseChannelRestorer) restoreTool(key, value string) (string, error) {
	session := r.tool[key]
	if session == nil {
		if len(r.text)+len(r.tool) >= 1024 {
			return "", fmt.Errorf("too many restoration channels")
		}
		session = r.e.NewRestoreSessionJSON()
		r.tool[key] = session
		r.toolAccum[key] = &bytes.Buffer{}
	}
	if r.buffered+int64(len(value)) > r.e.MaxRestoreBuffer() {
		return "", fmt.Errorf("tool arguments exceed restoration buffer")
	}
	r.toolAccum[key].WriteString(value)
	r.buffered += int64(len(value))
	return "", nil
}

func (r *sseChannelRestorer) finishPrefix(prefix string) error {
	if r.bad {
		return fmt.Errorf("tool argument contains an unresolvable placeholder; stream terminated")
	}
	for key, session := range r.tool {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		buf := r.toolAccum[key]
		var restored []byte
		var err error
		if json.Valid(buf.Bytes()) {
			restored, err = r.e.RestoreToolJSON(r.ctx, buf.Bytes())
		} else {
			restored, err = session.Push(r.ctx, buf.Bytes())
			if err == nil {
				var tail []byte
				tail, err = session.Finish(r.ctx)
				restored = append(restored, tail...)
			}
		}
		if err != nil {
			r.bad = true
			return err
		}
		if int64(len(restored)) > r.e.MaxRestoreBuffer() {
			return fmt.Errorf("restored tool arguments exceed restoration buffer")
		}
		if len(restored) > 0 {
			template := r.toolTemplates[key]
			if template.root == nil {
				return fmt.Errorf("missing tool delta template")
			}
			event, err := json.Marshal(deltaTail(template.root, template.path, string(restored)))
			if err != nil {
				return err
			}
			r.tails = append(r.tails, []byte("data: ")...)
			r.tails = append(r.tails, event...)
			r.tails = append(r.tails, '\n', '\n')
		}
		r.findings = append(r.findings, r.e.CheckToolArgs(string(restored))...)
		r.buffered -= int64(buf.Len())
		delete(r.tool, key)
		delete(r.toolAccum, key)
		delete(r.toolTemplates, key)
	}
	keys := make([]string, 0, len(r.text))
	for key := range r.text {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		tail, err := r.text[key].Finish(r.ctx)
		if err != nil {
			return err
		}
		if len(tail) > 0 {
			template := r.templates[key]
			if template.root == nil {
				return fmt.Errorf("missing text delta template")
			}
			event, err := json.Marshal(deltaTail(template.root, template.path, string(tail)))
			if err != nil {
				return err
			}
			r.tails = append(r.tails, []byte("data: ")...)
			r.tails = append(r.tails, event...)
			r.tails = append(r.tails, '\n', '\n')
		}
		delete(r.text, key)
		delete(r.templates, key)
	}
	return nil
}

// deltaTail copies only the selected channel and protocol selectors, excluding
// usage, finish reasons, and unrelated choices or tool calls.
func deltaTail(v any, path []string, tail string) any {
	if len(path) == 0 {
		return tail
	}
	switch node := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for _, key := range []string{"id", "type", "object", "model", "created", "index", "output_index", "content_index", "item_id"} {
			if value, ok := node[key]; ok {
				out[key] = value
			}
		}
		out[path[0]] = deltaTail(node[path[0]], path[1:], tail)
		return out
	case []any:
		n, _ := strconv.Atoi(path[0])
		return []any{deltaTail(node[n], path[1:], tail)}
	}
	return v
}

func (r *sseChannelRestorer) finish() error { return r.finishPrefix("") }
func (r *sseChannelRestorer) abort() {
	r.text = nil
	r.tool = nil
	r.toolAccum = nil
	r.templates = nil
	r.toolTemplates = nil
	r.tails = nil
}

type SSERestoreHooks struct {
	Engine     *redact.Engine
	OnError    func(error)
	OnFindings func([]redact.ToolFinding)
}

func CopySSERestored(ctx context.Context, w http.ResponseWriter, src io.Reader, o *Observer, hooks SSERestoreHooks) error {
	r := newSSEChannelRestorer(ctx, hooks.Engine)
	br := bufio.NewReader(src)
	controller := http.NewResponseController(w)
	fail := func(err error) error {
		if hooks.OnError != nil {
			hooks.OnError(err)
		}
		r.abort()
		return err
	}
	write := func(data []byte) error {
		if len(data) == 0 {
			return nil
		}
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		return controller.Flush()
	}
	var event []byte
	continuedLine := false
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		line, err := br.ReadSlice('\n')
		blankLine := !continuedLine && len(line) > 0 && len(bytes.TrimRight(line, "\r\n")) == 0
		continuedLine = err == bufio.ErrBufferFull
		if int64(len(event)+len(line)) > hooks.Engine.MaxRestoreBuffer() {
			return fail(fmt.Errorf("SSE event exceeds restoration buffer"))
		}
		event = append(event, line...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if blankLine {
			o.Write(event)
			if o.terminal {
				if err := r.finish(); err != nil {
					return fail(err)
				}
			}
			out, restoreErr := r.feedEvent(event)
			if restoreErr != nil {
				return fail(restoreErr)
			}
			if err := write(r.tails); err != nil {
				return fail(err)
			}
			r.tails = nil
			if err := write(out); err != nil {
				return fail(err)
			}
			event = nil
			if o.terminal {
				if hooks.OnFindings != nil {
					hooks.OnFindings(r.findings)
				}
				return nil
			}
		}
		if err != nil {
			if err != io.EOF {
				return fail(err)
			}
			// An unterminated event is never forwarded as a valid terminal event.
			if len(event) > 0 {
				o.Write(event)
			}
			if err := r.finish(); err != nil {
				return fail(err)
			}
			if err := write(r.tails); err != nil {
				return fail(err)
			}
			if hooks.OnFindings != nil {
				hooks.OnFindings(r.findings)
			}
			return nil
		}
	}
}

func (r *sseChannelRestorer) feedEvent(event []byte) ([]byte, error) {
	lines := bytes.SplitAfter(event, []byte("\n"))
	var payload []byte
	dataCount := 0
	for _, line := range lines {
		if data, ok := dataLine(line); ok {
			if dataCount > 0 {
				payload = append(payload, '\n')
			}
			payload = append(payload, data...)
			dataCount++
		}
	}
	if dataCount == 0 || string(payload) == "[DONE]" || len(payload) == 0 {
		return event, nil
	}
	restored, err := r.feedPayload(payload)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(restored, payload) {
		return event, nil
	}
	var out bytes.Buffer
	written := false
	for _, line := range lines {
		if _, ok := dataLine(line); ok {
			if !written {
				out.WriteString("data: ")
				out.Write(restored)
				if bytes.HasSuffix(line, []byte("\r\n")) {
					out.WriteString("\r\n")
				} else {
					out.WriteByte('\n')
				}
				written = true
			}
		} else {
			out.Write(line)
		}
	}
	return out.Bytes(), nil
}
