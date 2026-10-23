package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"safuno/internal/sessions"
)

const dshTitleFrame = "Generate the session title from this JSON array of human messages:"
const openCodeTitleFrame = "Generate a title for this conversation:"

// Only infrastructure system/developer instructions and explicit purpose fields
// classify a request. A user asking for an article title is ordinary model work.
func inspectTitleRequest(r *http.Request, path string, body []byte) *sessions.Title {
	if r.Method != http.MethodPost || (path != "/v1/chat/completions" && path != "/v1/messages" && path != "/v1/responses") {
		return nil
	}
	var p struct {
		System       json.RawMessage `json:"system"`
		Instructions json.RawMessage `json:"instructions"`
		Tools        json.RawMessage `json:"tools"`
		Messages     []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Metadata struct {
			Purpose string `json:"purpose"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &p) != nil {
		return nil
	}
	detector := ""
	purpose := func(s string) bool {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "title", "session-title", "session_title", "generate_session_title":
			return true
		}
		return false
	}
	if values := r.Header.Values("X-Request-Purpose"); len(values) == 1 && purpose(values[0]) {
		detector = "header:x-request-purpose"
	} else if len(values) == 1 && strings.TrimSpace(values[0]) != "" {
		return nil
	}
	if detector == "" && purpose(p.Metadata.Purpose) {
		detector = "body:metadata.purpose"
	} else if detector == "" && p.Metadata.Purpose != "" {
		return nil
	}
	instructions := []string{sessions.TextContent(p.System), sessions.TextContent(p.Instructions)}
	for _, m := range p.Messages {
		if m.Role == "system" || m.Role == "developer" {
			instructions = append(instructions, sessions.TextContent(m.Content))
		}
	}
	signature := ""
	for _, system := range instructions {
		system = strings.TrimSpace(system)
		switch {
		case strings.HasPrefix(system, "Create a concise title for an AI coding-assistant session") && strings.Contains(system, "supplied human messages"):
			signature = "dsh:session-title"
		case strings.HasPrefix(system, "You are a title generator. You output ONLY a thread title."):
			signature = "opencode:title-agent"
		case strings.HasPrefix(system, "Generate a concise, sentence-case title") && strings.Contains(system, "coding session") && strings.Contains(system, "\"title\""):
			signature = "claude-code:session-title"
		}
		if signature != "" {
			break
		}
	}
	tools := strings.TrimSpace(string(p.Tools))
	if signature == "" && (tools == "" || tools == "null" || tools == "[]" || tools == "{}") {
		for _, system := range instructions {
			lower := strings.ToLower(strings.TrimSpace(system))
			lead := lower
			if len(lead) > 200 {
				lead = lead[:200]
			}
			dedicated := strings.HasPrefix(lead, "generate ") || strings.HasPrefix(lead, "create ") || strings.HasPrefix(lead, "you are a title generator")
			scope := strings.Contains(lead, "session") || strings.Contains(lead, "conversation") || strings.Contains(lead, "thread") || strings.Contains(lead, "chat")
			only := strings.Contains(lower, "return only") || strings.Contains(lower, "output only") || strings.Contains(lower, "single line") || strings.Contains(lower, "single \"title\"")
			if dedicated && scope && only && strings.Contains(lead, "title") {
				signature = "compat:dedicated-title-prompt"
				break
			}
			if (strings.HasPrefix(lower, "为以下对话生成") || strings.HasPrefix(lower, "为这段对话生成") || strings.HasPrefix(lower, "生成一个会话标题")) && (strings.Contains(lower, "只返回标题") || strings.Contains(lower, "只输出标题")) {
				signature = "compat:dedicated-title-prompt"
				break
			}
		}
	}
	if detector == "" {
		detector = signature
	}
	if detector == "" {
		return nil
	}
	human := sessions.HumanMessages(path, body)
	switch signature {
	case "dsh:session-title":
		var messages []struct {
			Text string `json:"text"`
		}
		if len(human) == 1 && strings.HasPrefix(strings.TrimSpace(human[0]), dshTitleFrame) {
			input := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(human[0]), dshTitleFrame))
			if json.Unmarshal([]byte(input), &messages) == nil {
				human = nil
				for _, m := range messages {
					human = append(human, m.Text)
				}
			} else {
				human = nil
			}
		} else {
			human = nil
		}
	case "opencode:title-agent":
		if len(human) > 0 && strings.HasPrefix(strings.TrimSpace(human[0]), openCodeTitleFrame) {
			first := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(human[0]), openCodeTitleFrame))
			human = append([]string{}, human[1:]...)
			if first != "" {
				human = append([]string{first}, human...)
			}
		}
	case "claude-code:session-title":
		if len(human) == 1 {
			text := strings.TrimSpace(human[0])
			if strings.HasPrefix(text, "<session>") && strings.HasSuffix(text, "</session>") {
				human = []string{strings.TrimSuffix(strings.TrimPrefix(text, "<session>"), "</session>")}
			} else {
				human = nil
			}
		} else {
			human = nil
		}
	}
	return &sessions.Title{Detector: detector, Status: "pending", SourceMessages: sessions.MessageHashes(human)}
}
