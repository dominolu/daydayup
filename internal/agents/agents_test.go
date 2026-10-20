package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleOpenCode = `{
  // 注释必须保留
  "model": "old-model",
  "provider": "custom",
  "flags": {"x": 1}
}
`

func TestPatchOpenCodePreserves(t *testing.T) {
	out, err := PatchOpenCode([]byte(sampleOpenCode), "deepseek-v4.1-flash", "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "// 注释必须保留") {
		t.Fatalf("comment lost:\n%s", s)
	}
	if !strings.Contains(s, `"provider": "custom"`) || !strings.Contains(s, `"flags": {"x": 1}`) {
		t.Fatalf("unrelated fields changed:\n%s", s)
	}
	if !strings.Contains(s, `"model": "deepseek-v4.1-flash"`) {
		t.Fatalf("model not set:\n%s", s)
	}
	var doc map[string]any
	if err := json.Unmarshal(maskJSONComments(out), &doc); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, s)
	}
}

func TestPatchOpenCodeInsertWhenMissing(t *testing.T) {
	out, err := PatchOpenCode([]byte("{}\n"), "m1", "")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "m1" {
		t.Fatalf("model = %v", doc["model"])
	}
}

func TestPatchClaudeCode(t *testing.T) {
	src := `{
  "env": {"OTHER": "keep", "ANTHROPIC_BASE_URL": "https://api.anthropic.com"},
  "model": "claude-old",
  "extra": [1, 2]
}
`
	out, err := PatchClaudeCode([]byte(src), "http://127.0.0.1:8080", "gkey", "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	env := doc["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8080" {
		t.Fatalf("base url: %v", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "gkey" {
		t.Fatalf("token: %v", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if env["OTHER"] != "keep" {
		t.Fatalf("other env lost: %v", env)
	}
	if doc["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("model: %v", doc["model"])
	}
	if len(doc["extra"].([]any)) != 2 {
		t.Fatalf("extra lost")
	}
}

func TestPatchCodex(t *testing.T) {
	src := "# keep this comment\nmodel_provider = \"openai\"\nmodel = \"gpt-4o\"\n\n[model_providers.openai]\nname = \"OpenAI\"\n"
	out, err := PatchCodex([]byte(src), "http://127.0.0.1:8080", "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "# keep this comment") {
		t.Fatalf("comment lost:\n%s", s)
	}
	if !strings.Contains(s, `model_provider = "magpie"`) {
		t.Fatalf("provider not set:\n%s", s)
	}
	if !strings.Contains(s, `model = "deepseek-v4.1-flash"`) {
		t.Fatalf("model not set:\n%s", s)
	}
	if !strings.Contains(s, `[model_providers.magpie]`) || !strings.Contains(s, `base_url = "http://127.0.0.1:8080/v1"`) || !strings.Contains(s, `wire_api = "responses"`) {
		t.Fatalf("magpie section missing:\n%s", s)
	}
	// The original openai section must still exist.
	if !strings.Contains(s, `[model_providers.openai]`) {
		t.Fatalf("existing section dropped:\n%s", s)
	}
}

func TestPatchCline(t *testing.T) {
	src := `{
	"editor.fontSize": 14
}
`
	out, err := PatchCline([]byte(src), "http://127.0.0.1:8080", "m1", "k1")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["cline.apiProvider"] != "openai-compatible" || doc["cline.openAiModelId"] != "m1" || doc["cline.openAiBaseUrl"] != "http://127.0.0.1:8080/v1" || doc["cline.openAiKey"] != "k1" {
		t.Fatalf("cline keys wrong: %v", doc)
	}
	if doc["editor.fontSize"] != float64(14) {
		t.Fatalf("existing setting lost")
	}
}

func TestStoreApplyRestoreDrift(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "agents.json")
	s, err := NewStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(target, []byte(`{"model": "old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	agent, _ := Lookup("opencode")
	b := &Binding{AgentID: "opencode", Model: "deepseek-v4.1-flash", Mode: ModeAuto, TargetFile: target}
	content, _, res, err := s.Apply(agent, b, ApplyOptions{BaseURL: "http://127.0.0.1:8080", GatewayKey: "gk"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.BackedUp || res.BackupID == "" {
		t.Fatalf("expected backup, got %+v", res)
	}
	var doc map[string]any
	if err := json.Unmarshal(content, &doc); err != nil || doc["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("written content wrong: %v (%v)", doc, err)
	}
	// Drift detection: modify the file behind the gateway's back.
	if err := os.WriteFile(target, []byte(`{"model": "tampered"}`), 0600); err != nil {
		t.Fatal(err)
	}
	status, note := s.CurrentStatus(*b)
	if status != StatusDrift {
		t.Fatalf("expected drift, got %s (%s)", status, note)
	}
	// Apply must refuse to overwrite drifted file.
	if _, _, _, err := s.Apply(agent, b, ApplyOptions{BaseURL: "http://127.0.0.1:8080", GatewayKey: "gk"}); err == nil || !strings.Contains(err.Error(), "漂移") {
		t.Fatalf("expected drift refusal, got %v", err)
	}
	// Restore brings back the original file.
	if err := s.Restore(b); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(target)
	if string(restored) != `{"model": "old"}` {
		t.Fatalf("restore failed: %s", restored)
	}
}

func TestStorePersists(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "agents.json")
	s, err := NewStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update("opencode", func(b *Binding) { b.Model = "m9"; b.Status = StatusOK }); err != nil {
		t.Fatal(err)
	}
	s2, err := NewStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if b := s2.Binding("opencode"); b == nil || b.Model != "m9" {
		t.Fatalf("binding not persisted: %+v", b)
	}
}

func TestSnippet(t *testing.T) {
	snippet, err := Snippet("dsh", "http://127.0.0.1:8080", "gk", "m1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ANTHROPIC_BASE_URL='http://127.0.0.1:8080'", "ANTHROPIC_API_KEY='gk'", "OPENAI_BASE_URL='http://127.0.0.1:8080/v1'"} {
		if !strings.Contains(snippet, want) {
			t.Fatalf("snippet missing %q:\n%s", want, snippet)
		}
	}
	if _, err := Snippet("dsh", "http://x", "", "m1"); err == nil {
		t.Fatalf("expected error without key")
	}
}

func TestResolve(t *testing.T) {
	if _, err := Resolve("relative/path"); err == nil {
		t.Fatalf("relative path must be rejected")
	}
	p, err := Resolve("~/x.json")
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(p, home) {
		t.Fatalf("resolve: %s", p)
	}
}
func TestPatchIgnoresKeyInsideStringValue(t *testing.T) {
	src := `{
  "system_prompt": "use {\"model\": \"x\"} literally",
  "provider": "custom"
}
`
	out, err := PatchOpenCode([]byte(src), "deepseek-v4.1-flash", "")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(maskJSONComments(out), &doc); err != nil {
		t.Fatalf("output corrupt: %v\n%s", err, out)
	}
	if doc["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("model not set: %v", doc)
	}
	if !strings.Contains(string(out), `use {\"model\": \"x\"} literally`) {
		t.Fatalf("string value corrupted:\n%s", out)
	}
	if doc["provider"] != "custom" || !strings.Contains(string(out), `"provider": "custom"`) {
		t.Fatalf("unrelated field corrupted:\n%s", out)
	}
}

func TestBackupsNotReusedAcrossApplies(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(target, []byte(`{"model": "v0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	agent, _ := Lookup("opencode")
	opts := ApplyOptions{BaseURL: "http://x", GatewayKey: "k"}
	b1 := &Binding{AgentID: "opencode", Model: "m1", Mode: ModeAuto, TargetFile: target}
	_, _, res1, _, err := s.PersistApply(agent, b1, opts)
	if err != nil || !res1.BackedUp {
		t.Fatalf("apply1: %+v %v", res1, err)
	}
	b2 := s.Binding("opencode")
	b3 := &Binding{AgentID: "opencode", Model: "m2", Mode: ModeAuto, TargetFile: target}
	_, _, res2, _, err := s.PersistApply(agent, b3, opts)
	if err != nil || !res2.BackedUp {
		t.Fatalf("apply2: %+v %v", res2, err)
	}
	if res1.BackupID == res2.BackupID {
		t.Fatalf("backup ids reused: %s", res1.BackupID)
	}
	// The first backup must still hold the ORIGINAL v0 content.
	first, err := os.ReadFile(filepath.Join(s.Dir(), res1.BackupID))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != `{"model": "v0"}` {
		t.Fatalf("original backup lost: %s", first)
	}
	// Restoring the FIRST backup returns v0 even after two applies.
	b := s.Binding("opencode")
	b.BackupID = res1.BackupID
	if err := s.Restore(b); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(target)
	if string(restored) != `{"model": "v0"}` {
		t.Fatalf("restore returned %s", restored)
	}
	_ = b2
}


func TestPatchNestedKeyNotShadowed(t *testing.T) {
	src := `{
  "hooks": {"model": "nested"},
  "flags": {"x": 1}
}
`
	out, err := PatchOpenCode([]byte(src), "top-model", "")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(maskJSONComments(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "top-model" {
		t.Fatalf("top-level model must be set, got %v in:\n%s", doc["model"], out)
	}
	// nested hooks.model untouched
	if !strings.Contains(string(out), `"model": "nested"`) {
		t.Fatalf("nested model corrupted:\n%s", out)
	}
}
