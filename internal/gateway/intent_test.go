package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"safuno/internal/config"
)

func intentTestGateway(t *testing.T, classifyErr bool, configInt intentCfg) (*httptest.Server, string, *sync.Mutex, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"classifier"},{"id":"cheap"},{"id":"big"}]}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		seen = append(seen, body.Model)
		mu.Unlock()
		if body.Model == "classifier" && classifyErr {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"type":"server_error"}}`))
			return
		}
		content := "ok"
		if body.Model == "classifier" {
			content = "simple"
		}
		w.Header().Set("Content-Type", "application/json")
		reply, _ := json.Marshal(map[string]any{
			"id": "x", "model": body.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		})
		_, _ = w.Write(reply)
	})
	gw := httptest.NewServer(up)
	t.Cleanup(gw.Close)
	c := config.Config{DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 20, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, Intent: config.IntentConfig{Enabled: true, ClassifierModel: configInt.classifier, SimpleModels: configInt.simple, StrongModels: configInt.strong, Models: configInt.models}}
	c.Accounts = append(c.Accounts, config.Account{ID: "a1", Provider: "sensenova", BaseURL: gw.URL, APIKey: "upstream-secret-1", MaxInFlight: 8})
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	s.setAgentSelfCheckURL(server.URL)
	return server, c.GatewayKey, &mu, &seen
}

type intentCfg struct {
	classifier string
	simple     []string
	strong     []string
	models     []string
}

func TestIntentRoutingRoutesToSimple(t *testing.T) {
	server, key, mu, seen := intentTestGateway(t, false, intentCfg{classifier: "classifier", simple: []string{"cheap"}, strong: []string{"big"}, models: []string{"intent"}})
	req, _ := http.NewRequest("POST", server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"intent","messages":[{"role":"user","content":"天气怎么样"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	// Order: classifier call first, then the routed cheap model.
	if len(*seen) != 2 || (*seen)[0] != "classifier" || (*seen)[1] != "cheap" {
		t.Fatalf("unexpected upstream calls: %v", *seen)
	}
}

func TestIntentRoutingFallsBackToStrongOnClassifierError(t *testing.T) {
	server, key, mu, seen := intentTestGateway(t, true, intentCfg{classifier: "classifier", simple: []string{"cheap"}, strong: []string{"big"}, models: []string{"intent"}})
	req, _ := http.NewRequest("POST", server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"intent","messages":[{"role":"user","content":"写一个编译器"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	// classifier failed (500) then fallback to strong "big".
	if len(*seen) != 2 || (*seen)[1] != "big" {
		t.Fatalf("expected fallback to strong, got %v", *seen)
	}
}

func TestIntentModelListed(t *testing.T) {
	server, key, _, _ := intentTestGateway(t, false, intentCfg{classifier: "classifier", simple: []string{"cheap"}, strong: []string{"big"}, models: []string{"intent"}})
	req, _ := http.NewRequest("GET", server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"id":"intent"`) {
		t.Fatalf("intent model not listed: %s", b)
	}
}
func TestIntentRunsBeforeModelMap(t *testing.T) {
	// model_map must not swallow the intent trigger name, and the chosen
	// simple model must then pass through the model_map rename.
	var seenModels []string
	var mu sync.Mutex
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"classifier"},{"id":"cheap"},{"id":"big"}]}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		seenModels = append(seenModels, body.Model)
		mu.Unlock()
		content := "ok"
		if body.Model == "classifier" {
			content = "simple"
		}
		w.Header().Set("Content-Type", "application/json")
		reply, _ := json.Marshal(map[string]any{"id": "x", "model": body.Model, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
		_, _ = w.Write(reply)
	})
	gw := httptest.NewServer(up)
	t.Cleanup(gw.Close)
	c := config.Config{DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 20, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3,
		Intent: config.IntentConfig{Enabled: true, ClassifierModel: "classifier", SimpleModels: []string{"cheap"}, StrongModels: []string{"big"}, Models: []string{"intent"}},
		Middleware: config.MiddlewareConfig{ModelMap: map[string]string{"cheap": "cheap-renamed", "intent": "NOPE"}}, // "intent" must NOT be renamed before classification
	}
	c.Accounts = append(c.Accounts, config.Account{ID: "a1", Provider: "sensenova", BaseURL: gw.URL, APIKey: "upstream-secret-1", MaxInFlight: 8})
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	s.setAgentSelfCheckURL(server.URL)
	req, _ := http.NewRequest("POST", server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"intent","messages":[{"role":"user","content":"简单问题"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	// intent triggered the classifier, the chosen cheap model was renamed.
	if len(seenModels) != 2 || seenModels[0] != "classifier" {
		t.Fatalf("intent not classified first: %v", seenModels)
	}
	if seenModels[1] != "cheap-renamed" {
		t.Fatalf("model_map did not apply to chosen model: %v", seenModels)
	}
}
