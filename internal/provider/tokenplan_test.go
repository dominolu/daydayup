package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"safuno/internal/config"
)

func TestTokenPlanAuthorizeSetsBearer(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// The gateway strips client credentials (Authorization / X-API-Key /
	// Cookie) before Authorize runs, so the provider only owns setting its own.
	TokenPlan{}.Authorize(r, config.Account{APIKey: "tp-key"})
	if got := r.Header.Get("Authorization"); got != "Bearer tp-key" {
		t.Fatalf("authorization = %q", got)
	}
}

func TestTokenPlanModelsMergesGenerationAndDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tp-key" {
			t.Errorf("missing bearer auth")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-v4-flash-0731","object":"model"},{"id":"glm-5.3","object":"model"}],"models":[{"name":"intern-decision-4b","description":"决策"}]}`))
	}))
	defer srv.Close()

	models, err := TokenPlan{}.Models(context.Background(), srv.Client(), config.Account{BaseURL: srv.URL, APIKey: "tp-key"})
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("want 3 models, got %d: %v", len(models), models)
	}
	seen := map[string]Model{}
	for _, m := range models {
		seen[m["id"].(string)] = m
	}
	for _, want := range []string{"deepseek-v4-flash-0731", "glm-5.3", "intern-decision-4b"} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("missing model %q", want)
		}
	}
	if seen["intern-decision-4b"]["systemone_only"] != true {
		t.Fatal("decision model must be flagged systemone_only")
	}
	if _, ok := seen["glm-5.3"]["systemone_only"]; ok {
		t.Fatal("generation model must not be flagged systemone_only")
	}
}

func TestTokenPlanModelsRejectsUnexpectedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()
	if _, err := (TokenPlan{}).Models(context.Background(), srv.Client(), config.Account{BaseURL: srv.URL}); err == nil {
		t.Fatal("non-list body must be rejected")
	}
}

func TestTokenPlanModelsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := TokenPlan{}.Models(context.Background(), srv.Client(), config.Account{BaseURL: srv.URL})
	he, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("want HTTPError, got %T: %v", err, err)
	}
	if he.Status != http.StatusTooManyRequests || he.RetryAfter != "7" {
		t.Fatalf("unexpected http error: %+v", he)
	}
}

func TestTokenPlanQuotaUnsupported(t *testing.T) {
	if _, err := (TokenPlan{}).Quota(context.Background(), http.DefaultClient, config.Account{}); err != ErrQuotaUnsupported {
		t.Fatalf("want ErrQuotaUnsupported, got %v", err)
	}
}

func TestTokenPlanErrorHooksNeverScopedToModel(t *testing.T) {
	p := TokenPlan{}
	for _, body := range []string{
		`{"error":{"code":"rate_limit_exceeded"}}`,
		`{"error":{"code":"model_not_available"}}`,
		`{"type":"error","error":{"type":"rate_limit_error"}}`,
	} {
		if p.ModelRateLimit([]byte(body)) {
			t.Fatalf("ModelRateLimit must stay false for %s", body)
		}
		if p.ModelUnavailable([]byte(body)) {
			t.Fatalf("ModelUnavailable must stay false for %s", body)
		}
	}
}
