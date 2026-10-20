package gateway

import (
	"encoding/json"
	"mime"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"
)

// Observe only rate-limit diagnostics, never the complete response header set.
// Accept the common metric/scope orders without allowing credential headers
// such as X-RateLimit-Api-Key through a broad prefix match.
func isRateLimitHeader(name string) bool {
	name = strings.ToLower(name)
	switch name {
	case "retry-after", "retry-after-ms", "x-retry-after-ms", "ratelimit", "ratelimit-policy":
		return true
	}
	for _, prefix := range []string{"ratelimit-", "x-ratelimit-", "x-rate-limit-", "anthropic-ratelimit-"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		for _, metric := range []string{"limit", "remaining", "reset", "policy"} {
			if suffix == metric {
				return true
			}
			for _, scope := range []string{"requests", "tokens", "input-tokens", "output-tokens", "requests-priority", "tokens-priority", "tpm", "rpm", "itpm", "otpm", "unified"} {
				if suffix == metric+"-"+scope || suffix == scope+"-"+metric {
					return true
				}
			}
		}
	}
	return false
}

func observeRateLimitHeaders(h http.Header, clean func(string) string) (map[string][]string, bool) {
	const maxHeaders, maxValues, maxBytes = 32, 4, 4096
	headers := make(map[string][]string)
	keys := make([]string, 0, len(h))
	for key := range h {
		if isRateLimitHeader(key) {
			keys = append(keys, key)
		}
	}
	// Preserve the retry instruction before spending the bounded header budget.
	sort.Slice(keys, func(i, j int) bool {
		a, b := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		if (a == "retry-after") != (b == "retry-after") {
			return a == "retry-after"
		}
		return keys[i] < keys[j]
	})
	remaining, truncated := maxBytes, false
	for _, key := range keys {
		name := http.CanonicalHeaderKey(key)
		for _, raw := range h[key] {
			if len(headers[name]) >= maxValues || (headers[name] == nil && len(headers) >= maxHeaders) {
				truncated = true
				continue
			}
			value := clean(raw)
			if utf8.RuneCountInString(raw) > 256 {
				truncated = true
			}
			cost := len(name) + len(value)
			if cost > remaining {
				truncated = true
				continue
			}
			headers[name] = append(headers[name], value)
			remaining -= cost
		}
	}
	return headers, truncated
}

// Extract numeric output caps without changing the payload or retaining text.
// Multiple caps are preserved independently; no upstream precedence is assumed.
func requestOutputLimits(r *http.Request, body []byte) map[string]int64 {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "application/json" && !strings.HasSuffix(ct, "+json") {
		return nil
	}
	var payload struct {
		MaxTokens           json.RawMessage `json:"max_tokens"`
		MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
		MaxOutputTokens     json.RawMessage `json:"max_output_tokens"`
		GenerationConfig    json.RawMessage `json:"generationConfig"`
		GenerationConfigAlt json.RawMessage `json:"generation_config"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	limits := make(map[string]int64)
	add := func(key string, raw json.RawMessage) {
		var n *int64
		if len(raw) > 0 && json.Unmarshal(raw, &n) == nil && n != nil && *n >= 0 {
			limits[key] = *n
		}
	}
	add("max_tokens", payload.MaxTokens)
	add("max_completion_tokens", payload.MaxCompletionTokens)
	add("max_output_tokens", payload.MaxOutputTokens)
	for key, raw := range map[string]json.RawMessage{"generationConfig": payload.GenerationConfig, "generation_config": payload.GenerationConfigAlt} {
		var config map[string]json.RawMessage
		if json.Unmarshal(raw, &config) == nil {
			add(key+".maxOutputTokens", config["maxOutputTokens"])
			add(key+".max_output_tokens", config["max_output_tokens"])
		}
	}
	if len(limits) == 0 {
		return nil
	}
	return limits
}
