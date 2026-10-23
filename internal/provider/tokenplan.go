package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"safuno/internal/config"
)

// TokenPlan is the 书生·端砚 Intern InkStone model service (墨点计费).
//
// The platform speaks three wire protocols on one origin
// (https://discovery-api.intern-ai.org.cn): OpenAI Chat Completions /
// Responses under /v1, Anthropic Messages at /v1/messages and the structured
// SystemOne decision API at /v1/systemone. All of them authenticate with the
// same TokenPlan API Key {{SECRET_ksyqzf}} Authorization: Bearer, so the gateway can pass
// client paths through unchanged.
//
// Model discovery returns two lists in one document: generation models under
// data[].id (Chat / Responses / Messages) and decision models under
// models[].name (SystemOne only). Both are advertised so the union catalog is
// honest, but a model being visible does not mean it serves every endpoint.
type TokenPlan struct{}

func (TokenPlan) Name() string { return "tokenplan" }

func init() {
	config.SetEndpoint("tokenplan", config.EndpointSpec{Origin: "https://discovery-api.intern-ai.org.cn"})
}

var _ Descriptor = TokenPlan{}

// PlatformInfo is the admin-console description of Intern-ai (TokenPlan).
func (TokenPlan) PlatformInfo() Info {
	return Info{
		ID:          "tokenplan",
		Name:        "Intern-ai",
		Tagline:     "书生·端砚 · 墨点额度",
		Logo:        "logo-tokenplan",
		IDPrefix:    "intern",
		BaseURL:     "https://discovery-api.intern-ai.org.cn",
		Credential:  "API Key",
		Placeholder: "…",
		Hint:        "在 Intern-ai（TokenPlan）控制台创建 API Key（墨点计费，单人最多 10 把）。",
		Auth:        "api_key",
		Quota:       "unsupported",
	}
}


func (TokenPlan) Authorize(r *http.Request, a config.Account) {
	r.Header.Set("Authorization", "Bearer "+a.APIKey)
}

// Models discovers the models this API key can reach. The platform caps the
// document at a few hundred entries; anything larger is treated as invalid
// rather than parsed unbounded.
func (TokenPlan) Models(ctx context.Context, c *http.Client, a config.Account) ([]Model, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	TokenPlan{}.Authorize(r, a)
	resp, err := c.Do(r)
	if err != nil {
		return nil, errors.New("model discovery transport failure")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, &HTTPError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(b) > 4<<20 {
		return nil, errors.New("model discovery response too large or interrupted")
	}
	var data struct {
		Object string  `json:"object"`
		Data   []Model `json:"data"`
		Models []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			ReleaseDate string `json:"release_date"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &data); err != nil || data.Object != "list" {
		return nil, errors.New("invalid model discovery response")
	}
	models := make([]Model, 0, len(data.Data)+len(data.Models))
	for _, m := range data.Data {
		id, ok := m["id"].(string)
		if !ok || id == "" {
			return nil, errors.New("model discovery returned invalid model id")
		}
		models = append(models, m)
	}
	for _, m := range data.Models {
		if m.Name == "" {
			return nil, errors.New("model discovery returned invalid decision model")
		}
		entry := Model{"id": m.Name, "object": "model", "systemone_only": true}
		if m.Description != "" {
			entry["description"] = m.Description
		}
		if m.ReleaseDate != "" {
			entry["release_date"] = m.ReleaseDate
		}
		models = append(models, entry)
	}
	return models, nil
}

// Quota: TokenPlan exposes 墨点 balance and usage only through the signed-in
// console; the API-key surface documented by the platform has no balance
// endpoint. Observation therefore reports unsupported instead of inventing a
// zero balance, and operators can feed a trusted snapshot via quota_feed_url.
func (TokenPlan) Quota(ctx context.Context, c *http.Client, a config.Account) (Quota, error) {
	return Quota{}, ErrQuotaUnsupported
}

// ModelRateLimit: RPM / TPM and the 5h / 7d 墨点 caps are per user and shared
// by every API key, so a 429 cools the whole account (the gateway already
// honours Retry-After); it must not be scoped to one model.
func (TokenPlan) ModelRateLimit(body []byte) bool { return false }

// ModelUnavailable: model_not_available arrives as HTTP 404, while the 403
// family (api_key_disabled / user_disabled / whitelist_required) is account
// level, never model scoped, so nothing here denies a single model.
func (TokenPlan) ModelUnavailable(body []byte) bool { return false }
