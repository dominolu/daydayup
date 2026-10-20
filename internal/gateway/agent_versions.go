package gateway

// M4 agent version hints (P2-5): the Agents page can probe each local CLI's
// version with a bounded command run. Probes are explicit (a button press),
// cached in memory for an hour, and never run at list time.

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// versionProbe describes how to read a CLI's version.
type versionProbe struct {
	Binary string
	Args   []string
	// UpdateHint is a human hint for upgrading this agent.
	UpdateHint string
}

var versionProbes = map[string]versionProbe{
	"opencode":  {Binary: "opencode", Args: []string{"--version"}, UpdateHint: "按官方方式更新（如 brew 或转发的安装脚本）"},
	"claude-code": {Binary: "claude", Args: []string{"--version"}, UpdateHint: "claude update 或按官方安装方式"},
	"codex":     {Binary: "codex", Args: []string{"--version"}, UpdateHint: "codex update 或按官方安装方式"},
}

type agentVersionRecord struct {
	Agent      string    `json:"agent"`
	Binary     string    `json:"binary"`
	Version    string    `json:"version,omitempty"`
	Detected   bool      `json:"detected"`
	Error      string    `json:"error,omitempty"`
	UpdateHint string    `json:"update_hint,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

type versionCache struct {
	mu      sync.Mutex
	records map[string]agentVersionRecord
}

func newVersionCache() *versionCache {
	return &versionCache{records: map[string]agentVersionRecord{}}
}

// probeAgentVersion runs the binary once with a short timeout and records the
// result. It returns nil for agents without a probe (e.g. dsh, cline).
func (s *Server) probeAgentVersion(ctx context.Context, agentID string) *agentVersionRecord {
	probe, ok := versionProbes[agentID]
	if !ok {
		return nil
	}
	record := agentVersionRecord{Agent: agentID, Binary: probe.Binary, UpdateHint: probe.UpdateHint, CheckedAt: time.Now()}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, probe.Binary, probe.Args...)
	out, err := cmd.Output()
	if err != nil && probeCtx.Err() != nil {
		record.Error = "探测超时（5 秒）"
	} else if err != nil {
		record.Error = "未检测到该 CLI（可能未安装或不在 PATH）"
	} else {
		text := strings.TrimSpace(string(out))
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[:i]
		}
		if len(text) > 64 {
			text = text[:64]
		}
		if text != "" {
			record.Version = text
			record.Detected = true
		}
	}
	s.versions.mu.Lock()
	s.versions.records[agentID] = record
	s.versions.mu.Unlock()
	return &record
}

// cachedAgentVersion returns the cached probe when it is younger than ttl.
func (s *Server) cachedAgentVersion(agentID string, ttl time.Duration) *agentVersionRecord {
	s.versions.mu.Lock()
	defer s.versions.mu.Unlock()
	r, ok := s.versions.records[agentID]
	if !ok || time.Since(r.CheckedAt) > ttl {
		return nil
	}
	cp := r
	return &cp
}