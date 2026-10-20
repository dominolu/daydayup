package observe

import (
	"encoding/json"
)

// Content-bearing deltas qualify. Roles, keep-alives and usage packets do not.
func generatedDelta(root map[string]json.RawMessage, typ string) bool {
	nonempty := func(b json.RawMessage) bool { var value string; return json.Unmarshal(b, &value) == nil && value != "" }
	var choices []struct {
		Text  string `json:"text"`
		Delta struct {
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
			Refusal          string          `json:"refusal"`
			ToolCalls        []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			FunctionCall struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function_call"`
		} `json:"delta"`
	}
	if json.Unmarshal(root["choices"], &choices) == nil {
		for _, c := range choices {
			d := c.Delta
			if c.Text != "" || nonempty(d.Content) || d.ReasoningContent != "" || d.Reasoning != "" || d.Refusal != "" || d.FunctionCall.Name != "" || d.FunctionCall.Arguments != "" {
				return true
			}
			for _, tool := range d.ToolCalls {
				if tool.Function.Name != "" || tool.Function.Arguments != "" {
					return true
				}
			}
		}
	}
	switch typ {
	case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta", "response.refusal.delta":
		return nonempty(root["delta"])
	case "content_block_delta":
		var d struct {
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		}
		if json.Unmarshal(root["delta"], &d) == nil {
			return d.Text != "" || d.Thinking != "" || d.PartialJSON != ""
		}
	case "content_block_start":
		var block struct {
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
			Type     string `json:"type"`
			Name     string `json:"name"`
		}
		if json.Unmarshal(root["content_block"], &block) == nil {
			return block.Text != "" || block.Thinking != "" || (block.Type == "tool_use" && block.Name != "")
		}
	}
	return false
}

func (o *Observer) generated() {
	if o.firstGenerated.IsZero() {
		o.firstGenerated = o.observedAt
	}
	o.lastGenerated = o.observedAt
}
