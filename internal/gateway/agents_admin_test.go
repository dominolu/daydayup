package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"safuno/internal/config"
)

// newAgentTestGateway builds a gateway with the agents layer enabled but no
// upstream accounts; the agent admin surface must still list and bind.
func newAgentTestGateway(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	c := config.Config{
		InstanceName:    "test",
		GatewayKey:      "gateway-key-1234567890",
		AdminKey:        "admin-key-0987654321",
		StateFile:       filepath.Join(dir, "state.json"),
		DBFile:          filepath.Join(dir, "gateway.db"),
		MaxAttempts:     3,
		DefaultProvider: "sensenova",
	}
	c.Agents = config.AgentsConfig{Enabled: true, WriteMode: "auto", GatewayURL: "http://127.0.0.1:9999"}
	s := New(c)
	if err := s.LoadAgents(); err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	return s, dir
}

func adminRequest(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestAgentsListWhenDisabled(t *testing.T) {
	c := config.Config{GatewayKey: "gateway-key-1234567890", AdminKey: "admin-key-0987654321", MaxAttempts: 3, DefaultProvider: "sensenova"}
	s := New(c)
	rec := adminRequest(t, s, http.MethodGet, "/admin/v1/agents", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Enabled {
		t.Fatalf("agents should be disabled")
	}
}

func TestAgentsBindPreviewAndApply(t *testing.T) {
	s, dir := newAgentTestGateway(t)
	target := filepath.Join(dir, "opencode.json")
	// The file exists with prior content so restore has a real backup to go back to.
	if err := os.WriteFile(target, []byte("{\n  \"model\": \"initial\",\n  \"extra\": 1\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Preview must not touch the file.
	rec := adminRequest(t, s, http.MethodPost, "/admin/v1/agents/opencode/bind", map[string]any{
		"model": "deepseek-v4.1-flash", "mode": "auto", "target_file": target, "preview": true,
	})
	if rec.Code != 200 {
		t.Fatalf("preview status %d: %s", rec.Code, rec.Body.String())
	}
	var preview struct {
		Preview bool   `json:"preview"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.Preview || preview.Content == "" {
		t.Fatalf("preview missing content: %+v", preview)
	}
	contentBefore, _ := os.ReadFile(target)
	if string(contentBefore) != "{\n  \"model\": \"initial\",\n  \"extra\": 1\n}\n" {
		t.Fatalf("preview modified the file")
	}
	// Apply now writes the file and keeps a backup of the original.
	rec = adminRequest(t, s, http.MethodPost, "/admin/v1/agents/opencode/bind", map[string]any{
		"model": "deepseek-v4.1-flash", "mode": "auto", "target_file": target,
	})
	if rec.Code != 200 {
		t.Fatalf("apply status %d: %s", rec.Code, rec.Body.String())
	}
	var applied struct {
		OK       bool   `json:"ok"`
		BackedUp bool   `json:"backed_up"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if !applied.OK || !applied.BackedUp {
		t.Fatalf("apply result: %+v", applied)
	}
	if !bytes.Contains([]byte(applied.Content), []byte(`"model":"deepseek-v4.1-flash"`)) && !bytes.Contains([]byte(applied.Content), []byte(`"model": "deepseek-v4.1-flash"`)) {
		t.Fatalf("content missing model: %s", applied.Content)
	}
	// List shows the binding with status ok.
	rec = adminRequest(t, s, http.MethodGet, "/admin/v1/agents", nil)
	var list struct {
		Data []agentOverview `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range list.Data {
		if row.ID == "opencode" {
			found = true
			if row.Status != "ok" || row.Binding == nil {
				t.Fatalf("opencode row: %+v", row)
			}
		}
	}
	if !found {
		t.Fatalf("opencode missing from list")
	}
	// Drift: tamper with the file, list must show drift and re-apply refused.
	if err := os.WriteFile(target, []byte(`{"model": "tampered"}`), 0600); err != nil {
		t.Fatal(err)
	}
	rec = adminRequest(t, s, http.MethodGet, "/admin/v1/agents", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, row := range list.Data {
		if row.ID == "opencode" {
			if row.Status != "drift" {
				t.Fatalf("expected drift, got %s", row.Status)
			}
		}
	}
	rec = adminRequest(t, s, http.MethodPost, "/admin/v1/agents/opencode/bind", map[string]any{
		"model": "deepseek-v4.1-flash", "mode": "auto", "target_file": target,
	})
	if rec.Code != 400 {
		t.Fatalf("drift apply must be refused, got %d", rec.Code)
	}
	// Restore brings the file back to the pre-apply content.
	rec = adminRequest(t, s, http.MethodPost, "/admin/v1/agents/opencode/restore", nil)
	if rec.Code != 200 {
		t.Fatalf("restore status %d: %s", rec.Code, rec.Body.String())
	}
	restored, _ := os.ReadFile(target)
	if string(restored) != "{\n  \"model\": \"initial\",\n  \"extra\": 1\n}\n" {
		t.Fatalf("restore did not bring back original content: %q", restored)
	}
}

func TestAgentsSnippetAndCheck(t *testing.T) {
	s, _ := newAgentTestGateway(t)
	s.setAgentSelfCheckURL("http://127.0.0.1:0")
	rec := adminRequest(t, s, http.MethodPost, "/admin/v1/agents/dsh/bind", map[string]any{
		"model": "deepseek-v4.1-flash", "mode": "snippet",
	})
	if rec.Code != 200 {
		t.Fatalf("bind status %d: %s", rec.Code, rec.Body.String())
	}
	var bound struct {
		Snippet string `json:"snippet"`
		Wrote   bool   `json:"wrote"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bound); err != nil {
		t.Fatal(err)
	}
	if bound.Wrote {
		t.Fatalf("snippet mode must not write")
	}
	if !containsSub(bound.Snippet, "ANTHROPIC_BASE_URL") {
		t.Fatalf("snippet missing exports: %s", bound.Snippet)
	}
	// Self-check with an empty model flag must use the binding model and fail
	// cleanly when the gateway cannot be reached (model missing from list).
	rec = adminRequest(t, s, http.MethodPost, "/admin/v1/agents/dsh/check", map[string]any{})
	if rec.Code != 200 {
		t.Fatalf("check status %d: %s", rec.Code, rec.Body.String())
	}
	var chk struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chk); err != nil {
		t.Fatal(err)
	}
	if chk.Status != "model_missing" {
		t.Fatalf("expected model_missing (no upstream), got %s", chk.Status)
	}
}

func containsSub(s, sub string) bool {
	return len(s) >= len(sub) && (bytes.Contains([]byte(s), []byte(sub)))
}
func TestAgentsProfileApply(t *testing.T) {
	s, dir := newAgentTestGateway(t)
	target := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(target, []byte("{\"model\":\"initial\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Bind opencode, then save a profile capturing the current choice.
	rec := adminRequest(t, s, http.MethodPost, "/admin/v1/agents/opencode/bind", map[string]any{
		"model": "deepseek-v4.1-flash", "mode": "auto", "target_file": target,
	})
	if rec.Code != 200 {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body.String())
	}
	rec = adminRequest(t, s, http.MethodPut, "/admin/v1/agents/profiles", map[string]any{
		"name": "省钱", "agents": map[string]string{"opencode": "flash-lite"},
	})
	if rec.Code != 200 {
		t.Fatalf("save profile: %d %s", rec.Code, rec.Body.String())
	}
	// Apply the profile switches the model.
	rec = adminRequest(t, s, http.MethodPost, "/admin/v1/agents/profiles/省钱/apply", map[string]any{})
	if rec.Code != 200 {
		t.Fatalf("apply profile: %d %s", rec.Code, rec.Body.String())
	}
	var applied struct {
		Results []struct {
			Agent string `json:"agent"`
			OK    bool   `json:"ok"`
			Wrote bool   `json:"wrote"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if len(applied.Results) != 1 || !applied.Results[0].OK || !applied.Results[0].Wrote {
		t.Fatalf("apply results: %+v", applied.Results)
	}
	content, _ := os.ReadFile(target)
	if !bytes.Contains(content, []byte("flash-lite")) {
		t.Fatalf("profile did not switch model: %s", content)
	}
	// Delete the profile.
	rec = adminRequest(t, s, http.MethodDelete, "/admin/v1/agents/profiles/省钱", nil)
	if rec.Code != 200 {
		t.Fatalf("delete profile: %d", rec.Code)
	}
}
