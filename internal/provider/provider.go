// Package provider is the extension boundary for additional upstream platforms.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"safuno/internal/config"
)

var ErrQuotaUnsupported = errors.New("official API-key quota endpoint is not documented")

type Model map[string]any

// Quota is a timestamped observation, not a conversion from tokens to credits.
type Quota struct {
	Partial         bool      `json:"partial,omitempty"`
	Source          string    `json:"source"`
	ObservedAt      time.Time `json:"observed_at"`
	ValidUntil      time.Time `json:"valid_until"`
	RemainingTokens *int64    `json:"remaining_tokens"`
	Buckets         []Bucket  `json:"buckets,omitempty"`
}

type Bucket struct {
	Name      string     `json:"name"`
	Pool      string     `json:"pool"`
	Unit      string     `json:"unit"`
	Remaining float64    `json:"remaining"`
	Limit     float64    `json:"limit"`
	ResetsAt  *time.Time `json:"resets_at,omitempty"`
	Models    []string   `json:"models,omitempty"`
}

// CheckinResult is the outcome of a WorkBuddy daily check-in (CN realm credit
// claim). AlreadyDone means the upstream answered "already checked in today"
// (code 10001), which counts as success and records the date.
type CheckinResult struct {
	AlreadyDone bool   `json:"already_done,omitempty"`
	Message     string `json:"message,omitempty"`
}

// CheckinStatus is the daily check-in activity snapshot from the billing
// meter status endpoint (today_checked_in / streak / credit values).
type CheckinStatus struct {
	TodayCheckedIn bool    `json:"today_checked_in"`
	StreakDays     int     `json:"streak_days"`
	DailyCredit    float64 `json:"daily_credit"`
	TodayCredit    float64 `json:"today_credit"`
	Active         bool    `json:"active"`
}

// ActivityResult is the outcome of a CN active-report (growth streak event
// replay); StreakDays is the consecutive check-in days when the upstream
// answered the streak query.
type ActivityResult struct {
	StreakDays int    `json:"streak_days,omitempty"`
	Message    string `json:"message,omitempty"`
}

// DailyChatResult is the outcome of the intl daily activity chat (the
// desktop-identity light turn; the official daily-activity credit counts the
// web-channel turn, which this gateway does not drive).
type DailyChatResult struct {
	Message string `json:"message"`
}

// DailyTasks is an optional Provider capability: WorkBuddy / CodeBuddy bonus
// claiming — CN daily check-in, CN active report (growth streak) and the intl
// daily activity chat. The gateway schedules these once per day per account
// and exposes manual triggers; providers without the capability are skipped.
type DailyTasks interface {
	Checkin(ctx context.Context, client *http.Client, a config.Account) (CheckinResult, error)
	CheckinStatus(ctx context.Context, client *http.Client, a config.Account) (*CheckinStatus, error)
	ActivityReport(ctx context.Context, client *http.Client, a config.Account) (ActivityResult, error)
	DailyChat(ctx context.Context, client *http.Client, a config.Account) (DailyChatResult, error)
}

type Provider interface {
	Name() string
	Authorize(*http.Request, config.Account)
	Models(context.Context, *http.Client, config.Account) ([]Model, error)
	Quota(context.Context, *http.Client, config.Account) (Quota, error)
	ModelRateLimit([]byte) bool
	ModelUnavailable([]byte) bool
}

// SessionAuthorizer is an optional Provider capability: the gateway resolves
// the session (or generates one) before the upstream call and hands the
// session key and the requested model to the provider so it can emit
// session-derived and model-derived headers (e.g. WorkBuddy conversation IDs
// and the VSCode identity's X-Model-ID) that stay stable across a chat.
type SessionAuthorizer interface {
	AuthorizeSession(*http.Request, config.Account, string, string)
}

// PathRewriter is an optional Provider capability: it maps the client path
// to the upstream path (e.g. WorkBuddy serves chat at /v2/chat/completions).
type PathRewriter interface {
	RewritePath(clientPath string) (string, bool)
}

// Refreshable is an optional Provider capability: account credentials are
// rotated in place (OAuth access tokens). Refresh returns the account with
// updated tokens; the gateway persists and swaps it atomically.
type Refreshable interface {
	Refresh(context.Context, *http.Client, config.Account) (config.Account, error)
}

// QuotaExhausted is an optional Provider capability: an upstream error body
// that is a definitive "account balance exhausted" answer (WorkBuddy 402).
// The gateway parks the account until the reported reset time.
type QuotaExhausted interface {
	QuotaExhausted([]byte) bool
}

// Restriction is an optional Provider capability: an upstream 403 body that
// is an account-level abuse restriction. The gateway cools the whole realm
// so other accounts on the same derived device are not implicated.
type Restriction interface {
	Restriction([]byte) bool
}

// BodyTransformer is an optional Provider capability: the request body is
// rewritten for a specific upstream identity (e.g. gzip for WorkBuddy
// CLI/VSCode). EncodingHeaders carries headers that must accompany the
// rewritten body (Content-Encoding: gzip).
type BodyTransformer interface {
	TransformRequestBody(method, path string, contentType string, body []byte, a config.Account) ([]byte, error)
	BodyEncodingHeaders(a config.Account) http.Header
}

type SenseNova struct{}

// SenseNova uses the same insufficient_quota code for different limits. Only
// the observed explicit engine TPM/RPM error is scoped to the selected model;
// unrecognized limits retain conservative account-wide cooldown semantics.
func (SenseNova) ModelRateLimit(body []byte) bool {
	var data struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &data) == nil && data.Error.Type == "rate_limit_error" && data.Error.Message == "inference exceeds tpm/rpm limit"
}

func (SenseNova) ModelUnavailable(body []byte) bool {
	var data struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &data) == nil && data.Error.Type == "permission_denied_error" && data.Error.Message == "model is not available in the current token plan"
}

func (SenseNova) Name() string { return "sensenova" }

func init() {
	config.SetEndpoint("sensenova", config.EndpointSpec{Origin: "https://token.sensenova.cn"})
	// Declared here, in the package every adapter imports, so no adapter
	// package's init order can hide it. The WorkBuddy adapter refines the
	// declaration with its realm origins (and the realm lookup the CN base
	// URL implies) in its own init.
	config.SetRealmPlatform("workbuddy", []string{"intl", "cn"}, []string{"desktop", "vscode", "cli"}, nil)
}

var _ Descriptor = SenseNova{}

// PlatformInfo is the admin-console description of SenseNova (model discovery
// under /v1/models with a Bearer API key; quota only via an operator feed).
func (SenseNova) PlatformInfo() Info {
	return Info{
		ID:          "sensenova",
		Name:        "SenseNova",
		Tagline:     "商汤日日新 · 积分额度",
		Logo:        "logo-sensenova",
		IDPrefix:    "sense",
		BaseURL:     "https://token.sensenova.cn",
		Credential:  "API Key",
		Placeholder: "sk-…",
		Hint:        "在 SenseNova 控制台创建 API Key 后粘贴到这里。",
		Auth:        "api_key",
		Quota:       "snapshot",
	}
}

func (SenseNova) Authorize(r *http.Request, a config.Account) {
	r.Header.Set("Authorization", "Bearer "+a.APIKey)
}
func (s SenseNova) Models(ctx context.Context, c *http.Client, a config.Account) ([]Model, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	s.Authorize(r, a)
	resp, err := c.Do(r)
	if err != nil {
		return nil, errors.New("model discovery transport failure")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, &HTTPError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	}
	var data struct {
		Data []Model `json:"data"`
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(b) > 4<<20 {
		return nil, errors.New("model discovery response too large or interrupted")
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

// Optional feed is an operator-provided normalized source, not a SenseNova API.
// It has its own credential and never receives the upstream model API key.
func (SenseNova) Quota(ctx context.Context, c *http.Client, a config.Account) (Quota, error) {
	if a.QuotaFeedURL == "" {
		return Quota{}, ErrQuotaUnsupported
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, a.QuotaFeedURL, nil)
	if err != nil {
		return Quota{}, errors.New("invalid quota feed URL")
	}
	if a.QuotaFeedKey != "" {
		r.Header.Set("Authorization", "Bearer "+a.QuotaFeedKey)
	}
	resp, err := c.Do(r)
	if err != nil {
		return Quota{}, errors.New("quota feed transport failure")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Quota{}, fmt.Errorf("quota feed HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return Quota{}, errors.New("quota feed response too large or interrupted")
	}
	var q Quota
	if json.Unmarshal(b, &q) != nil {
		return Quota{}, errors.New("invalid quota feed JSON")
	}
	return q, nil
}

type HTTPError struct {
	Status     int
	RetryAfter string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("model discovery HTTP %d", e.Status) }
