package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"safuno/internal/config"
)

// Generic is the adapter used for platforms registered at runtime from the
// admin console. It implements only the OpenAI-compatible wire protocol the
// gateway already forwards verbatim:
//
//   - Authorization: Bearer <API key>
//   - model discovery at GET <base_url>/v1/models ({"data":[{"id":…}]})
//   - no path rewrite, no body transform, no session authorizer
//
// Platform-specific quirks (OAuth identities, conversation headers, gzip
// bodies, billing endpoints) still require a compiled-in adapter. Hot
// registration makes a new OpenAI-compatible upstream reachable, testable and
// removable without a restart.
type Generic struct{}

var _ Provider = Generic{}
var _ Descriptor = Generic{}

func (Generic) Name() string { return "generic" }

func (Generic) PlatformInfo() Info { return Info{Auth: "api_key", Quota: "unsupported"} }

func (Generic) Authorize(r *http.Request, a config.Account) {
	r.Header.Set("Authorization", "Bearer "+a.APIKey)
	r.Header.Del("X-Api-Key")
}

func (Generic) Models(ctx context.Context, c *http.Client, a config.Account) ([]Model, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, errors.New("invalid base URL")
	}
	Generic{}.Authorize(r, a)
	resp, err := c.Do(r)
	if err != nil {
		return nil, errors.New("model discovery transport failure")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(b) > 4<<20 {
		return nil, errors.New("model discovery response too large or interrupted")
	}
	var data struct {
		Data []Model `json:"data"`
	}
	if err = json.Unmarshal(b, &data); err != nil || data.Data == nil {
		return nil, errors.New("invalid model discovery response")
	}
	for _, m := range data.Data {
		if id, ok := m["id"].(string); !ok || id == "" {
			return nil, errors.New("model discovery returned invalid model id")
		}
	}
	return data.Data, nil
}

// Quota is unsupported: a generic platform has no documented balance API, so
// the console shows it as an unsupported feed instead of guessing.
func (Generic) Quota(context.Context, *http.Client, config.Account) (Quota, error) {
	return Quota{}, ErrQuotaUnsupported
}

// ModelRateLimit uses the OpenAI error type both platforms emit for a
// model-scoped rate limit. Corroborated by 429; unknown shapes keep the
// account-wide cooldown.
func (Generic) ModelRateLimit(body []byte) bool {
	var data struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &data) != nil {
		return false
	}
	return data.Error.Code == "rate_limit_exceeded" || data.Error.Type == "rate_limit_error" ||
		data.Error.Code == "model_rate_limited"
}

// ModelUnavailable recognizes the OpenAI-compatible model-permission error:
// the credential is valid but this model is not in the account's plan.
func (Generic) ModelUnavailable(body []byte) bool {
	var data struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &data) != nil {
		return false
	}
	code := data.Error.Code
	if code == "" {
		code = data.Error.Type
	}
	switch code {
	case "model_not_found", "model_not_available", "permission_denied_error", "model_not_in_token_plan":
		return true
	}
	return strings.Contains(strings.ToLower(data.Error.Message), "model is not available")
}
