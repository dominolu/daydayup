package agents

import (
	"fmt"
	"os"
	"time"
)

// ApplyOptions carries the gateway values injected into generated configs.
// They are passed per call and never persisted.
type ApplyOptions struct {
	BaseURL    string // gateway origin, e.g. http://127.0.0.1:8080 (no /v1)
	GatewayKey string // client gateway key
}

// BuildContent produces the file content for an auto-write binding, applying
// the surgical patch on top of the current file (or a fresh document when the
// file does not exist yet). snippet is the human-readable fragment that the
// UI can show for snippet mode.
func (s *Store) BuildContent(agent Agent, b Binding, opts ApplyOptions) (content []byte, snippet string, err error) {
	if b.Model == "" {
		return nil, "", fmt.Errorf("请先为 %s 选择模型", agent.Name)
	}
	if opts.GatewayKey == "" {
		return nil, "", fmt.Errorf("网关客户端 Key 为空（启动环境变量 GATEWAY_API_KEY）")
	}
	if b.Mode == ModeSnippet {
		snippet, err = Snippet(agent.ID, opts.BaseURL, opts.GatewayKey, b.Model)
		return nil, snippet, err
	}
	target, err := Resolve(b.TargetFile)
	if err != nil {
		return nil, "", err
	}
	existing, err := ReadConfig(target)
	if err != nil {
		return nil, "", err
	}
	if existing == nil {
		// Fresh document: start from the snippet shape so the file is complete.
		switch agent.ID {
		case "opencode":
			existing = []byte("{\n}\n")
		case "claude-code":
			existing = []byte("{\n}\n")
		case "cline":
			existing = []byte("{\n}\n")
		case "codex":
			existing = []byte("")
		}
	}
	switch agent.ID {
	case "opencode":
		content, err = PatchOpenCode(existing, b.Model, "")
	case "claude-code":
		content, err = PatchClaudeCode(existing, opts.BaseURL, opts.GatewayKey, b.Model)
	case "cline":
		content, err = PatchCline(existing, opts.BaseURL, b.Model, opts.GatewayKey)
	case "codex":
		content, err = PatchCodex(existing, opts.BaseURL, b.Model)
	case "dsh":
		snippet, err = Snippet(agent.ID, opts.BaseURL, opts.GatewayKey, b.Model)
		return nil, snippet, err
	default:
		err = fmt.Errorf("Agent %q 不支持自动写入", agent.ID)
	}
	if err != nil {
		return nil, "", err
	}
	return content, "", nil
}

// CurrentStatus re-reads the bound config file and classifies drift.
// It returns the status and a human-readable note.
func (s *Store) CurrentStatus(b Binding) (string, string) {
	if b.Mode == ModeSnippet || b.Model == "" {
		if b.Model == "" {
			return StatusUnbound, "尚未选择模型"
		}
		return StatusSnippet, "仅生成配置片段，未写入文件"
	}
	if b.TargetFile == "" {
		return StatusError, "未配置目标文件"
	}
	target, err := Resolve(b.TargetFile)
	if err != nil {
		return StatusError, err.Error()
	}
	content, err := ReadConfig(target)
	if err != nil {
		return StatusMissing, "无法读取配置文件：" + err.Error()
	}
	if content == nil {
		return StatusMissing, "配置文件不存在：" + target
	}
	if b.FileDigest != "" && Digest(content) != b.FileDigest {
		return StatusDrift, "配置文件已被外部工具修改，重新接入前请先还原"
	}
	return StatusOK, "配置文件与上次写入一致"
}

// Apply writes the binding's config to disk with backup, updating the binding
// state. It refuses to overwrite a drifted file (the caller must restore first).
func (s *Store) Apply(agent Agent, b *Binding, opts ApplyOptions) (content []byte, snippet string, res *ApplyResult, err error) {
	if b.Mode == ModeSnippet {
		snippet, err = Snippet(agent.ID, opts.BaseURL, opts.GatewayKey, b.Model)
		if err != nil {
			return nil, "", nil, err
		}
		b.Status = StatusSnippet
		b.LastError = ""
		return nil, snippet, nil, nil
	}
	target, err := Resolve(b.TargetFile)
	if err != nil {
		return nil, "", nil, err
	}
	b.TargetFile = target
	content, snippet, err = s.BuildContent(agent, *b, opts)
	if err != nil {
		return nil, "", nil, err
	}
	// Drift guard: never silently overwrite a file changed outside the gateway.
	status, note := s.CurrentStatus(*b)
	if status == StatusDrift {
		return nil, "", nil, fmt.Errorf("检测到配置漂移：%s。请先还原备份再重新接入", note)
	}
	res, err = s.WriteFile(target, "", content)
	if err != nil {
		b.Status = StatusError
		b.LastError = err.Error()
		return nil, "", nil, err
	}
	b.AppliedAt = now()
	b.FileDigest = Digest(content)
	b.BackupID = res.BackupID
	b.Status = StatusOK
	b.LastError = ""
	return content, "", res, nil
}

// PersistApply applies and persists the binding under the store lock, so the
// resulting FileDigest/BackupID/Status survive. On apply error the binding's
// mutated state is still saved (a failed write keeps the previous digest, so
// drift/error statuses are preserved). Returns the updated binding.
func (s *Store) PersistApply(agent Agent, b *Binding, opts ApplyOptions) (content []byte, snippet string, res *ApplyResult, updated *Binding, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.binding[b.AgentID] = b
	content, snippet, res, err = s.Apply(agent, b, opts)
	if saveErr := s.saveLocked(); err == nil && saveErr != nil {
		err = saveErr
	}
	return content, snippet, res, cloneBinding(b), err
}

// Restore puts the last backup back over the target file and clears the
// binding's write state.
func (s *Store) Restore(b *Binding) error {
	if b.BackupID == "" {
		return fmt.Errorf("该 Agent 没有可还原的备份（从未自动写入或备份已清理）")
	}
	target, err := Resolve(b.TargetFile)
	if err != nil {
		return err
	}
	if err := s.RestoreFile(target, b.BackupID); err != nil {
		return err
	}
	b.FileDigest = ""
	b.BackupID = ""
	b.Status = StatusUnbound
	b.LastError = ""
	return nil
}

// HasFile reports whether the resolved target exists on disk.
func HasFile(target string) (bool, error) {
	t, err := Resolve(target)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(t); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

var now = func() time.Time { return time.Now() }
