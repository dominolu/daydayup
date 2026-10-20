package redact

import "encoding/json"

// CheckToolJSON observes complete tool arguments in non-streaming responses.
func (e *Engine) CheckToolJSON(body []byte) []ToolFinding {
	var root any
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	var findings []ToolFinding
	var walk func(any)
	walk = func(v any) {
		switch node := v.(type) {
		case []any:
			for _, item := range node {
				walk(item)
			}
		case map[string]any:
			if args, ok := node["arguments"].(string); ok {
				findings = append(findings, e.CheckToolArgs(args)...)
			}
			if node["type"] == "tool_use" {
				if args, ok := node["input"].(map[string]any); ok {
					b, _ := json.Marshal(args)
					findings = append(findings, e.CheckToolArgs(string(b))...)
				}
			}
			for _, item := range node {
				walk(item)
			}
		}
	}
	walk(root)
	return findings
}
