package gateway

// M4 intent routing (P2-1): when the requested model is an intent trigger
// (default "intent"), a tiny classifier call decides between a cheap and a
// strong concrete model, then the request body is rewritten before the pool
// picks an account. The classifier rides the same gateway (self-loop) so it
// benefits from pooling, redaction and sessions; any classifier failure falls
// back to the strong model (safe default). The classifier model is validated
// to never be a trigger, so there is no recursion.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// intentTrigger reports whether the requested model activates classification.
func (s *Server) intentTrigger(model string) bool {
	if !s.Config.Intent.Enabled || model == "" {
		return false
	}
	for _, m := range s.Config.Intent.Models {
		if m == model {
			return true
		}
	}
	return false
}

// applyIntent classifies and rewrites the request body when the model is an
// intent trigger. It returns the new body and whether routing happened.
func (s *Server) applyIntent(r *http.Request, trace *requestTrace, body []byte) ([]byte, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body, false
	}
	model := ""
	if raw, ok := root["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	if !s.intentTrigger(model) {
		return body, false
	}
	// Hard recursion guard (config.Load validates this too; direct Config
	// construction must not be able to classify itself forever).
	if model == s.Config.Intent.ClassifierModel {
		return body, false
	}
	userText := lastUserMessage(root)
	chosen, source, err := s.classify(r, userText)
	if err != nil {
		// Fall back to the strong model; never fail the client for it.
		chosen, source = s.Config.Intent.StrongModels[0], "fallback_error"
		slog.Warn("intent classifier failed; using strong model", "error", err, "request_id", trace.ID)
	}
	if chosen == "" {
		return body, false
	}
	b, _ := json.Marshal(chosen)
	root["model"] = b
	out, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	slog.Info("intent routed", "request_id", trace.ID, "client_model", model, "chosen", chosen, "source", source)
	return out, true
}

// classify sends one tiny non-streaming chat request through the gateway with
// the classifier model and returns the routed model id.
func (s *Server) classify(r *http.Request, userText string) (model, source string, err error) {
	cfg := s.Config.Intent
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 || maxTokens > 16 {
		maxTokens = 8
	}
	prompt := cfg.SystemPromptText
	if prompt == "" {
		prompt = "判断用户的这条消息是否需要深度推理或复杂工具调用。只需回答 simple 或 strong。"
	}
	msg := userText
	if msg == "" {
		msg = "（无文本消息）"
	}
	body, _ := json.Marshal(map[string]any{
		"model":     cfg.ClassifierModel,
		"messages":  []map[string]string{{"role": "system", "content": prompt}, {"role": "user", "content": msg}},
		"max_tokens": maxTokens,
		"stream":    false,
	})
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.agentSelfCheckURL()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Session-ID", "intent-classifier")
	started := time.Now()
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("classifier HTTP %d", resp.StatusCode)
	}
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &chat) == nil && len(chat.Choices) > 0 {
		answer := strings.ToLower(strings.TrimSpace(chat.Choices[0].Message.Content))
		switch {
		case strings.Contains(answer, "simple"):
			return cfg.SimpleModels[0], fmt.Sprintf("simple (%d ms)", time.Since(started).Milliseconds()), nil
		case strings.Contains(answer, "strong"):
			return cfg.StrongModels[0], fmt.Sprintf("strong (%d ms)", time.Since(started).Milliseconds()), nil
		default:
			return cfg.StrongModels[0], "strong_unknown", nil
		}
	}
	return "", "", fmt.Errorf("classifier returned no answer")
}

// lastUserMessage extracts the most recent user turn text from a Chat request.
func lastUserMessage(root map[string]json.RawMessage) string {
	raw, ok := root["messages"]
	if !ok {
		return ""
	}
	var messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &messages) != nil {
		return ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}
