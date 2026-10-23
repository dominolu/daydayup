package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"safuno/internal/config"
)

// A canonical client model that a platform renames from its own upstream id
// must be routed under the canonical name, forwarded as the platform id, and
// returned to the client under the canonical name.
func TestModelRenameSendsUpstreamIDAndReturnsCanonical(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		reply, _ := json.Marshal(map[string]any{
			"id": "c1", "model": body.Model, "object": "chat.completion",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
		})
		_, _ = w.Write(reply)
	}))
	t.Cleanup(up.Close)
	c := config.Config{
		DefaultProvider: "sensenova", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16",
		RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3,
		DBFile:      filepath.Join(t.TempDir(), "gateway.db"),
		ModelRename: map[string]map[string]string{"sensenova": {"deepseek-flash": "deepseek-v4.1-flash-1017"}},
	}
	c.Accounts = []config.Account{{ID: "a0", Provider: "sensenova", BaseURL: up.URL, APIKey: "k0", MaxInFlight: 8}}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, requestModel := range []string{"deepseek-v4.1-flash-1017", "deepseek-flash"} {
		rec := call(s, "POST", "/v1/chat/completions", `{"model":"`+requestModel+`","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != 200 {
			t.Fatalf("%s: status = %d body = %s", requestModel, rec.Code, rec.Body.String())
		}
		if gotModel != "deepseek-flash" {
			t.Fatalf("%s: upstream model = %q, want the platform's own id", requestModel, gotModel)
		}
		var resp struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Model != "deepseek-v4.1-flash-1017" {
			t.Fatalf("%s: client model = %q, want the canonical name", requestModel, resp.Model)
		}
	}
}
