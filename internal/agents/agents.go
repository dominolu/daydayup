// Package agents adapts local coding agents (DSH, Claude Code, Codex,
// OpenCode, Cline) to the gateway: it knows each agent's configuration file
// shape, builds the config snippet that points the agent at the gateway, and
// persists per-agent model bindings with backup / restore / drift detection.
//
// The package is pure: it never talks to the gateway or the network. The
// gateway layer owns the admin HTTP surface and the optional self-check call.
package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"daydayup/internal/store"
)

// Agent describes one supported client and how to wire it to the gateway.
type Agent struct {
	ID             string   `json:"id"`                        // stable id used in APIs and bindings
	Name           string   `json:"name"`                      // display name
	Hint           string   `json:"hint"`                      // human readable "where the config lives"
	RequiresBridge string   `json:"requires_bridge,omitempty"` // bridge flag the gateway must enable: "messages" | "responses"
	EnvVars        []string `json:"env_vars,omitempty"`        // environment variables the gateway key / base url ride on
}

// Catalog returns the supported agents in a stable order.
func Catalog() []Agent {
	return []Agent{
		{ID: "dsh", Name: "DeepSeek Harness", Hint: "环境变量 ANTHROPIC_BASE_URL / OPENAI_BASE_URL + ANTHROPIC_API_KEY（可写入 ~/.dsh/gateway.env）", EnvVars: []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "OPENAI_BASE_URL", "OPENAI_API_KEY"}},
		{ID: "opencode", Name: "OpenCode", Hint: "~/.config/opencode/opencode.json（model 字段）"},
		{ID: "claude-code", Name: "Claude Code", Hint: "~/.claude/settings.json（env + model 字段）", RequiresBridge: "messages"},
		{ID: "codex", Name: "Codex", Hint: "~/.codex/config.toml（model_providers.magpie 段）", RequiresBridge: "responses"},
		{ID: "cline", Name: "Cline", Hint: "VS Code User settings.json（cline.* 键，macOS: ~/Library/Application Support/Code/User/settings.json）"},
	}
}

// Lookup returns the agent with id, or an error naming the known ids.
func Lookup(id string) (Agent, error) {
	for _, a := range Catalog() {
		if a.ID == id {
			return a, nil
		}
	}
	return Agent{}, fmt.Errorf("未知 Agent %q（支持：%s）", id, strings.Join(Names(), "、"))
}

// Names returns the catalog ids.
func Names() []string {
	names := make([]string, 0, len(Catalog()))
	for _, a := range Catalog() {
		names = append(names, a.ID)
	}
	return names
}

// Mode values for Binding.Mode.
const (
	ModeAuto    = "auto"    // write the agent config file directly (default)
	ModeSnippet = "snippet" // only produce the snippet, never touch the file
)

// Status values for Binding.Status.
const (
	StatusUnbound = "unbound" // no model selected yet
	StatusSnippet = "snippet" // binding active in snippet mode (file untouched)
	StatusOK      = "ok"      // file on disk matches the last applied content
	StatusDrift   = "drift"   // file was changed outside the gateway since apply
	StatusMissing = "missing" // configured target file is gone
	StatusError   = "error"   // last apply failed
)

// Binding is the persisted model choice and write state for one agent.
// It stores no credentials: the gateway key is injected when a snippet or a
// config write is produced, never persisted here.
type Binding struct {
	AgentID    string    `json:"agent_id"`
	Model      string    `json:"model,omitempty"`       // upstream model id
	Provider   string    `json:"provider,omitempty"`    // provider namespace (display only)
	Mode       string    `json:"mode,omitempty"`        // auto (default) | snippet
	TargetFile string    `json:"target_file,omitempty"` // resolved config path (auto mode)
	AppliedAt  time.Time `json:"applied_at,omitempty"`  // last successful write/apply
	FileDigest string    `json:"file_digest,omitempty"` // sha256 of the file as last written
	BackupID   string    `json:"backup_id,omitempty"`   // backup name for restore
	Status     string    `json:"status,omitempty"`      // see Status constants
	LastError  string    `json:"last_error,omitempty"`  // last apply/check error
	LastCheck  time.Time `json:"last_check,omitempty"`  // last self-check time
	CheckOK    bool      `json:"check_ok,omitempty"`    // last self-check result
	CheckNote  string    `json:"check_note,omitempty"`  // last self-check detail (latency / error)
}

// Store persists bindings and profiles in the shared gateway database.
// Backups of user config files stay as files under <dbdir>/backups.
type Store struct {
	mu       sync.Mutex
	dbPath   string
	db       *store.DB
	dbOnce   sync.Once
	dbErr    error
	dir      string // directory holding backups
	binding  map[string]*Binding
	profiles map[string]*Profile
}

// Profile snapshots the model choice of every bound agent under a name.
// It stores no credentials and no whitelist — pure model selections.
type Profile struct {
	Name    string            `json:"name"`
	Agents  map[string]string `json:"agents"` // agentID -> model id
	SavedAt time.Time         `json:"saved_at"`
}

func (s *Store) open() (*store.DB, error) {
	s.dbOnce.Do(func() {
		s.db, s.dbErr = store.Open(s.dbPath)
	})
	return s.db, s.dbErr
}

// NewStore opens (creating if needed) the binding store inside the gateway
// database at dbPath. Backups are kept next to the database under
// <dbdir>/backups. A nil error is returned even when the database is fresh.
func NewStore(dbPath string) (*Store, error) {
	s := &Store{dbPath: dbPath, binding: map[string]*Binding{}, profiles: map[string]*Profile{}}
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("agents store directory: %w", err)
		}
	}
	s.dir = filepath.Join(filepath.Dir(dbPath), "backups")
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.SQL().Query(`SELECT agent_id, binding FROM agents_bindings ORDER BY agent_id`)
	if err != nil {
		return nil, fmt.Errorf("agents store read: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, binding string
		if err := rows.Scan(&id, &binding); err != nil {
			return nil, fmt.Errorf("agents store scan: %w", err)
		}
		var b Binding
		if err := json.Unmarshal([]byte(binding), &b); err != nil {
			return nil, fmt.Errorf("agents store corrupt: %w", err)
		}
		cb := b
		s.binding[id] = &cb
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	prows, err := db.SQL().Query(`SELECT name, profile FROM agents_profiles ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("agents store read: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var name, profile string
		if err := prows.Scan(&name, &profile); err != nil {
			return nil, fmt.Errorf("agents store scan: %w", err)
		}
		var p Profile
		if err := json.Unmarshal([]byte(profile), &p); err != nil {
			return nil, fmt.Errorf("agents store corrupt: %w", err)
		}
		cp := p
		s.profiles[name] = &cp
	}
	if err := prows.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir returns the backup directory (created on first use).
func (s *Store) Dir() string { return s.dir }

// Binding returns the stored binding for agentID (nil when unbound).
func (s *Store) Binding(agentID string) *Binding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneBinding(s.binding[agentID])
}

// Bindings returns every non-nil binding, sorted by agent id.
func (s *Store) Bindings() []Binding {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Binding, 0, len(s.binding))
	for _, b := range s.binding {
		if b != nil {
			out = append(out, *cloneBinding(b))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// Save atomically persists bindings. The gateway calls it after every change;
// the store also keeps the newest snapshot for crash recovery via sync.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	db, err := s.open()
	if err != nil {
		return fmt.Errorf("agents store write: %w", err)
	}
	tx, err := db.SQL().Begin()
	if err != nil {
		return fmt.Errorf("agents store write: %w", err)
	}
	defer tx.Rollback()
	for id, b := range s.binding {
		if b == nil {
			continue
		}
		payload, err := json.Marshal(b)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO agents_bindings(agent_id, binding) VALUES(?, ?) ON CONFLICT(agent_id) DO UPDATE SET binding = excluded.binding`, id, string(payload)); err != nil {
			return fmt.Errorf("agents store write: %w", err)
		}
	}
	for name, p := range s.profiles {
		if p == nil {
			continue
		}
		payload, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO agents_profiles(name, profile) VALUES(?, ?) ON CONFLICT(name) DO UPDATE SET profile = excluded.profile`, name, string(payload)); err != nil {
			return fmt.Errorf("agents store write: %w", err)
		}
	}
	return tx.Commit()
}

// Update applies fn to the binding of agentID, persists and returns the
// updated binding. fn may mutate the binding in place; the store re-saves.
func (s *Store) Update(agentID string, fn func(*Binding)) (*Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.binding[agentID]
	if b == nil {
		b = &Binding{AgentID: agentID, Mode: ModeAuto, Status: StatusUnbound}
	}
	fn(b)
	if b.Mode == "" {
		b.Mode = ModeAuto
	}
	if b.Mode != ModeAuto && b.Mode != ModeSnippet {
		return nil, fmt.Errorf("写入模式必须是 auto 或 snippet")
	}
	s.binding[agentID] = b
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneBinding(b), nil
}

// Remove clears the binding of agentID.
func (s *Store) Remove(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.binding[agentID]; !ok {
		return os.ErrNotExist
	}
	delete(s.binding, agentID)
	return s.saveLocked()
}

// Profiles returns all stored profiles sorted by name.
func (s *Store) Profiles() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Profile, 0, len(s.profiles))
	for _, p := range s.profiles {
		cp := *p
		cp.Agents = map[string]string{}
		for k, v := range p.Agents {
			cp.Agents[k] = v
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Profile returns one profile (nil when missing).
func (s *Store) Profile(name string) *Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.profiles[name]
	if p == nil {
		return nil
	}
	cp := *p
	cp.Agents = map[string]string{}
	for k, v := range p.Agents {
		cp.Agents[k] = v
	}
	return &cp
}

// SaveProfile creates or replaces a profile. agents maps agent id -> model.
func (s *Store) SaveProfile(name string, agents map[string]string) (*Profile, error) {
	if len(strings.TrimSpace(name)) == 0 || len([]rune(name)) > 40 || strings.ContainsAny(name, "\r\n\x00/\\") {
		return nil, fmt.Errorf("方案名称必须为 1–40 个字符，不能包含路径分隔符")
	}
	name = strings.TrimSpace(name)
	if len(agents) == 0 {
		return nil, fmt.Errorf("方案至少需要一个 Agent 的模型选择")
	}
	normalized := map[string]string{}
	for id, model := range agents {
		if _, err := Lookup(id); err != nil {
			return nil, err
		}
		if strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("Agent %s 未选择模型", id)
		}
		normalized[id] = strings.TrimSpace(model)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &Profile{Name: name, Agents: normalized, SavedAt: now()}
	s.profiles[name] = p
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneProfile(p), nil
}

// DeleteProfile removes a named profile.
func (s *Store) DeleteProfile(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.profiles[name]; !ok {
		return os.ErrNotExist
	}
	delete(s.profiles, name)
	return s.saveLocked()
}

// SnapshotBindings captures the current model choice of every bound agent into
// a map usable by SaveProfile (agents without a model are skipped).
func (s *Store) SnapshotBindings() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for id, b := range s.binding {
		if b != nil && b.Model != "" {
			out[id] = b.Model
		}
	}
	return out
}

func cloneProfile(p *Profile) *Profile {
	cp := *p
	cp.Agents = map[string]string{}
	for k, v := range p.Agents {
		cp.Agents[k] = v
	}
	return &cp
}

// Digest returns the lowercase hex sha256 of content.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Resolve expands a leading ~ to the current user's home directory.
func Resolve(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("目标文件为空")
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法解析主目录：%w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("目标文件必须是绝对路径或以 ~ 开头")
	}
	return filepath.Clean(path), nil
}

// DefaultTarget returns the conventional config path for an agent, or ""
// when the agent has no canonical single file (snippet-only defaults).
func DefaultTarget(id string) string {
	switch id {
	case "opencode":
		return "~/.config/opencode/opencode.json"
	case "claude-code":
		return "~/.claude/settings.json"
	case "codex":
		return "~/.codex/config.toml"
	case "cline":
		// VS Code keeps its User settings in different places per platform.
		switch runtime.GOOS {
		case "darwin":
			return "~/Library/Application Support/Code/User/settings.json"
		case "windows":
			if appdata := os.Getenv("APPDATA"); appdata != "" {
				return filepath.Join(appdata, "Code", "User", "settings.json")
			}
			return "~/AppData/Roaming/Code/User/settings.json"
		default:
			return "~/.config/Code/User/settings.json"
		}
	}
	return ""
}

// shellQuote wraps a value in single quotes for shell export lines, escaping
// embedded single quotes (export KEY='value').
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func cloneBinding(b *Binding) *Binding {
	if b == nil {
		return nil
	}
	c := *b
	return &c
}

// Snippet builds the configured text an agent needs to point at the gateway:
// environment exports for DSH-style agents, or a settings.json / config.toml
// fragment for file-based agents. The gateway key is passed in per call and
// never stored.
func Snippet(id, baseURL, gatewayKey, model string) (string, error) {
	if gatewayKey == "" {
		return "", fmt.Errorf("网关客户端 Key 为空，无法生成接入配置")
	}
	switch id {
	case "dsh":
		return fmt.Sprintf("# DeepSeek Harness 接入 DayDayUp 网关\n# 单机个人使用：把下面几行加入你的 shell 环境或 ~/.dsh/gateway.env\nexport ANTHROPIC_BASE_URL=%s\nexport ANTHROPIC_API_KEY=%s\nexport OPENAI_BASE_URL=%s\nexport OPENAI_API_KEY=%s\n# 默认模型（可选）\nexport DSH_DEFAULT_MODEL=%s\n", shellQuote(baseURL), shellQuote(gatewayKey), shellQuote(baseURL+"/v1"), shellQuote(gatewayKey), shellQuote(model)), nil
	case "opencode":
		return fmt.Sprintf("// OpenCode 接入：写入 ~/.config/opencode/opencode.json 后重启 OpenCode\n{\n  \"model\": %q\n}\n", model), nil
	case "claude-code":
		return fmt.Sprintf("// Claude Code 接入：写入 ~/.claude/settings.json（需开启 bridges.messages）\n{\n  \"env\": {\n    \"ANTHROPIC_BASE_URL\": %q,\n    \"ANTHROPIC_AUTH_TOKEN\": %q\n  },\n  \"model\": %q\n}\n", baseURL, gatewayKey, model), nil
	case "codex":
		return fmt.Sprintf("# Codex 接入：写入 ~/.codex/config.toml（需开启 bridges.responses）\nmodel_provider = \"magpie\"\nmodel = %q\n\n[model_providers.magpie]\nname = \"magpie\"\nbase_url = %q\nwire_api = \"responses\"\nenv_key = \"MAGPIE_API_KEY\"\n", model, baseURL+"/v1"), nil
	case "cline":
		return fmt.Sprintf("// Cline 接入：写入 VS Code User settings.json\n{\n  \"cline.apiProvider\": \"openai-compatible\",\n  \"cline.openAiBaseUrl\": %q,\n  \"cline.openAiModelId\": %q,\n  \"cline.openAiKey\": %q\n}\n", baseURL+"/v1", model, gatewayKey), nil
	}
	return "", fmt.Errorf("未知 Agent %q", id)
}
