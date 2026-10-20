package redact

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// transformJSONValues retains original bytes except for changed values. A nil
// replacement retains the original lexeme, including string escapes and
// number precision. Raw replacements also let numeric credentials roundtrip
// through a string placeholder without changing their restored JSON type.
func transformJSONValues(body []byte, visit func([]string, string, bool) ([]byte, error)) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("redact: invalid JSON document")
	}
	pos, last := 0, 0
	var out strings.Builder
	replace := func(path []string, start, end int, value string, quoted bool) error {
		next, err := visit(path, value, quoted)
		if err != nil {
			return err
		}
		if next != nil {
			out.Write(body[last:start])
			out.Write(next)
			last = end
		}
		return nil
	}
	skipSpace := func() {
		for pos < len(body) && jsonSpace(body[pos]) {
			pos++
		}
	}
	readString := func() (int, int, string) {
		start := pos
		pos++
		for body[pos] != '"' {
			if body[pos] == '\\' {
				pos++
			}
			pos++
		}
		pos++
		var value string
		_ = json.Unmarshal(body[start:pos], &value)
		return start, pos, value
	}
	var walk func([]string) error
	walk = func(path []string) error {
		skipSpace()
		switch body[pos] {
		case '"':
			start, end, value := readString()
			if err := replace(path, start, end, value, true); err != nil {
				return err
			}
		case '{':
			pos++
			skipSpace()
			for body[pos] != '}' {
				_, _, key := readString()
				skipSpace()
				pos++
				if err := walk(append(path, key)); err != nil {
					return err
				}
				skipSpace()
				if body[pos] == ',' {
					pos++
					skipSpace()
				} else {
					break
				}
			}
			pos++
		case '[':
			pos++
			skipSpace()
			for body[pos] != ']' {
				if err := walk(append(path, "*")); err != nil {
					return err
				}
				skipSpace()
				if body[pos] == ',' {
					pos++
					skipSpace()
				} else {
					break
				}
			}
			pos++
		default:
			start := pos
			for pos < len(body) && !strings.ContainsRune(",]} \r\n\t", rune(body[pos])) {
				pos++
			}
			if err := replace(path, start, pos, string(body[start:pos]), false); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(nil); err != nil {
		return nil, err
	}
	if last == 0 {
		return body, nil
	}
	out.Write(body[last:])
	return []byte(out.String()), nil
}

func changedJSONString(before, after string, err error) ([]byte, error) {
	if err != nil || before == after {
		return nil, err
	}
	return json.Marshal(after)
}

// This label is reserved for JSON number values, not a separate toggleable
// rule. SECRET controls it; the distinct label preserves number/string types.
const numericCredentialLabel = "SECRET_JSON_NUMBER"

func (e *Engine) secretRuleEnabled() bool {
	e.state.mu.RLock()
	defer e.state.mu.RUnlock()
	for _, r := range e.state.rules {
		if r.label == "SECRET" {
			return true
		}
	}
	return false
}

func jsonNumber(value string) bool {
	return value != "" && (value[0] == '-' || value[0] >= '0' && value[0] <= '9') && json.Valid([]byte(value))
}

func (e *Engine) maskJSONMode(ctx context.Context, body []byte, request bool, depth int) (string, int, map[string]uint64, error) {
	if depth > 16 {
		return "", 0, nil, fmt.Errorf("redact: embedded JSON nesting exceeds limit")
	}
	applied := 0
	entities := map[string]uint64{}
	visitString := func(path []string, value string) (string, error) {
		if request && protectedRequestField(path, value) {
			return value, nil
		}
		var masked string
		var count int
		var counts map[string]uint64
		var err error
		field := ""
		if len(path) > 0 {
			field = path[len(path)-1]
		}
		if (field == "arguments" || field == "user_id") && json.Valid([]byte(value)) {
			masked, count, counts, err = e.maskJSONMode(ctx, []byte(value), request && field == "user_id", depth+1)
		} else {
			if field == "*" {
				field = ""
			}
			masked, count, counts, err = e.maskText(ctx, value, field)
		}
		if err != nil {
			return "", err
		}
		applied += count
		for label, n := range counts {
			entities[label] += n
		}
		return masked, nil
	}
	out, err := transformJSONValues(body, func(path []string, value string, quoted bool) ([]byte, error) {
		if quoted {
			next, err := visitString(path, value)
			return changedJSONString(value, next, err)
		}
		if len(path) == 0 || !classifyCredentialLabel(path[len(path)-1]).known || !jsonNumber(value) || request && protectedRequestField(path, value) || !e.secretRuleEnabled() {
			return nil, nil
		}
		token, err := e.tm.TokenFor(numericCredentialLabel, value)
		if err != nil {
			return nil, err
		}
		e.emitSpan(ctx, SpanMasked, "SECRET", value, token)
		applied++
		entities["SECRET"]++
		return json.Marshal(token)
	})
	return string(out), applied, entities, err
}

// Preserve protocol identifiers, tool schemas and opaque media. Descriptions
// and tool arguments still run through the masking rules.
func protectedRequestField(path []string, value string) bool {
	if len(path) == 0 {
		return false
	}
	field := path[len(path)-1]
	joined := strings.Join(path, ".")
	// Nested tool input objects contain user data, even when their properties
	// happen to be named role, type, id or function.name.
	for i, part := range path {
		if i > 0 && part == "input" {
			return false
		}
	}
	if len(path) == 1 {
		switch field {
		case "model", "previous_response_id", "thread_id", "conversation", "conversation_id", "prompt_cache_key", "session_id":
			return true
		}
	}
	if joined == "metadata.session_id" || joined == "dsh_session_log.session.id" || joined == "session_id" || joined == "conversation.id" {
		return true
	}
	if (field == "url" && len(path) > 1 && path[len(path)-2] == "image_url") || field == "image_url" || field == "file_url" || field == "file_data" {
		return true
	}
	if field == "data" && len(path) > 1 && (path[len(path)-2] == "source" || path[len(path)-2] == "input_audio") {
		return true
	}
	if strings.HasPrefix(joined, "tools.") {
		if field == "description" {
			return false
		}
		if strings.Contains(joined, ".parameters.") || strings.Contains(joined, ".input_schema.") || field == "name" || field == "type" {
			return true
		}
	}
	if strings.HasPrefix(joined, "response_format.") || strings.HasPrefix(joined, "text.format.") {
		return field != "description"
	}
	switch field {
	case "role":
		return joined == "messages.*.role" || joined == "input.*.role"
	case "type":
		return joined == "messages.*.content.*.type" || joined == "input.*.type" || joined == "input.*.content.*.type" || joined == "tool_choice.type"
	case "tool_call_id":
		return joined == "messages.*.tool_call_id"
	case "call_id":
		return joined == "input.*.call_id"
	case "id":
		return joined == "messages.*.content.*.id" || joined == "input.*.id" || strings.Contains(joined, ".tool_calls.")
	case "name":
		return strings.Contains(joined, ".function.") || strings.Contains(joined, ".tool_calls.") || joined == "messages.*.name" || joined == "messages.*.content.*.name" || joined == "input.*.name"
	}
	return false
}

func (e *Engine) restoreJSON(ctx context.Context, body []byte, tool bool, depth int) ([]byte, error) {
	if depth > 16 {
		return nil, fmt.Errorf("redact: embedded JSON nesting exceeds limit")
	}
	visitString := func(path []string, value string) (string, error) {
		inTool := tool
		for _, part := range path {
			if part == "arguments" || part == "partial_json" || part == "input" {
				inTool = true
			}
		}
		field := ""
		if len(path) > 0 {
			field = path[len(path)-1]
		}
		if (field == "arguments" || field == "partial_json" || field == "user_id") && json.Valid([]byte(value)) {
			out, err := e.restoreJSON(ctx, []byte(value), inTool, depth+1)
			return string(out), err
		}
		if inTool && e.hasUnknownToken(ctx, value) {
			return "", fmt.Errorf("redact: unresolvable placeholder in tool arguments")
		}
		if inTool && (field == "arguments" || field == "partial_json") {
			out, err := e.restore(ctx, []byte(value), true)
			return string(out), err
		}
		out, err := e.Restore(ctx, []byte(value))
		return string(out), err
	}
	return transformJSONValues(body, func(path []string, value string, quoted bool) ([]byte, error) {
		if !quoted {
			return nil, nil
		}
		field := ""
		if len(path) > 0 {
			field = path[len(path)-1]
		}
		if label, original, ok := e.tm.Resolve(value); ok && label == numericCredentialLabel && (tool || classifyCredentialLabel(field).known) {
			if !jsonNumber(original) {
				return nil, fmt.Errorf("redact: invalid numeric credential mapping")
			}
			return e.Restore(ctx, []byte(value))
		}
		next, err := visitString(path, value)
		return changedJSONString(value, next, err)
	})
}

func (e *Engine) hasUnknownToken(ctx context.Context, value string) bool {
	unknown := uint64(0)
	for _, token := range placeholderRE.FindAllString(value, -1) {
		if _, _, ok := e.tm.Resolve(token); !ok {
			unknown++
		}
	}
	if unknown > 0 {
		e.bumpUnresolved(ctx, unknown)
	}
	return unknown > 0
}

func jsonSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }
