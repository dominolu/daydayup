package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"daydayup/internal/config"
)

func TestRoutingEndpoint(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}), 1)
	rec := call(s, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("proxy status %d", rec.Code)
	}
	routing := adminRequest(t, s, http.MethodGet, "/admin/v1/routing?limit=10", nil)
	if routing.Code != 200 {
		t.Fatalf("routing status %d", routing.Code)
	}
	var out struct {
		Decisions  []routeDecision `json:"decisions"`
		WaitActive int64           `json:"wait_active"`
	}
	if err := json.Unmarshal(routing.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Decisions) == 0 {
		t.Fatalf("no routing decisions recorded")
	}
	d := out.Decisions[0]
	if d.Model != "m" || d.Status != 200 || len(d.Hops) == 0 {
		t.Fatalf("decision: %+v", d)
	}
	if d.Hops[0].Account == "" || d.Hops[0].Status != 200 {
		t.Fatalf("hop: %+v", d.Hops[0])
	}
}

func TestUsageEndpoint(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}), 1)
	// Enable session storage so records feed the aggregation.
	s.Config.DBFile = filepath.Join(t.TempDir(), "gateway.db")
	if err := s.LoadSessions(); err != nil {
		t.Fatal(err)
	}
	// One regular call with a session id and one with a DSH identity header.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", "usage-test-1")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("call1 status %d", rec.Code)
	}
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-DeepSeek-Harness-Session-Id", "usage-test-2")
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("call2 status %d", rec2.Code)
	}
	usage := adminRequest(t, s, http.MethodGet, "/admin/v1/usage?window=24h", nil)
	if usage.Code != 200 {
		t.Fatalf("usage status %d", usage.Code)
	}
	var out struct {
		Rows struct {
			Clients []usageRow `json:"clients"`
			Models  []usageRow `json:"models"`
		} `json:"rows"`
		SessionEnabled bool `json:"session_storage_enabled"`
	}
	if err := json.Unmarshal(usage.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.SessionEnabled {
		t.Fatalf("sessions should be enabled")
	}
	if len(out.Rows.Clients) == 0 || len(out.Rows.Models) == 0 {
		t.Fatalf("no usage rows: %s", usage.Body.String())
	}
	// Totals across clients must be 20 input / 10 output.
	var in, outT, reqs int64
	for _, c := range out.Rows.Clients {
		in += c.Input
		outT += c.Output
		reqs += int64(c.Requests)
	}
	if in != 20 || outT != 10 || reqs != 2 {
		t.Fatalf("totals in=%d out=%d reqs=%d", in, outT, reqs)
	}
	if out.Rows.Clients[0].CostMode == "" {
		t.Fatalf("cost mode should be classified (unpriced here)")
	}
}

func TestUsagePriceTable(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"paid","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":1000,"total_tokens":2000}}`))
	}), 1)
	price := 10.0 // CNY per 1M tokens
	s.Config.Pricing = config.PricingConfig{"paid": {Input: &price, Output: &price}}
	s.Config.DBFile = filepath.Join(t.TempDir(), "gateway.db")
	if err := s.LoadSessions(); err != nil {
		t.Fatal(err)
	}
	call(s, "POST", "/v1/chat/completions", `{"model":"paid","messages":[{"role":"user","content":"hi"}]}`)
	usage := adminRequest(t, s, http.MethodGet, "/admin/v1/usage?window=24h", nil)
	var out struct {
		Rows struct {
			Models []usageRow `json:"models"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(usage.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Rows.Models) == 0 {
		t.Fatalf("no models")
	}
	m := out.Rows.Models[0]
	if m.CostMode != "price" {
		t.Fatalf("mode: %s", m.CostMode)
	}
	// input 1000 tokens = 0.001M * 10 = 0.01; output same -> 0.02 CNY.
	if m.Cost < 0.019 || m.Cost > 0.021 {
		t.Fatalf("cost %.6f", m.Cost)
	}
}
