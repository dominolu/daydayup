package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"daydayup/internal/agents"
	"daydayup/internal/pool"
)

// LoadAgents opens the agent binding store (M1). Disabled main switch or an
// empty store path leaves the store nil and the agents admin surface reports
// the feature as off.
func (s *Server) LoadAgents() error {
	if !s.Config.Agents.Enabled {
		return nil
	}
	st, err := agents.NewStore(s.Config.DBFile)
	if err != nil {
		return err
	}
	s.agentStore = st
	return nil
}

func (s *Server) agentEnabled() bool { return s.agentStore != nil }

// agentSelfCheckURL returns the URL the self-check uses to reach the gateway
// itself (overridable in tests via setAgentSelfCheckURL).
func (s *Server) agentSelfCheckURL() string {
	if s.agentSelfCheckOverride != "" {
		return s.agentSelfCheckOverride
	}
	return s.Config.Agents.GatewayURL
}

func (s *Server) setAgentSelfCheckURL(url string) { s.agentSelfCheckOverride = url }

// agentOverview is the shape returned by GET /admin/v1/agents.
type agentOverview struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Hint        string          `json:"hint"`
	Requires    string          `json:"requires_bridge,omitempty"`
	Binding     *agents.Binding `json:"binding,omitempty"`
	Status      string          `json:"status"`
	Installed   bool            `json:"installed"`
	TargetFile  string          `json:"target_file,omitempty"`
	DriftNote   string          `json:"drift_note,omitempty"`
	HasBackup   bool            `json:"has_backup"`
	SnippetText string          `json:"snippet,omitempty"`
}

// agentAdmin handles /admin/v1/agents*. It returns false to let the admin
// router fall through when the path does not belong to the agents surface.
func (s *Server) agentAdmin(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if path != "/admin/v1/agents" && !strings.HasPrefix(path, "/admin/v1/agents/") {
		return false
	}
	if !s.agentEnabled() {
		if path == "/admin/v1/agents" && r.Method == http.MethodGet {
			writeJSON(w, 200, map[string]any{"enabled": false, "data": []any{}})
			return true
		}
		apiError(w, 400, "agents_disabled", "Agent 接入未启用：配置 agents.enabled=true 后重启网关")
		return true
	}
	if path == "/admin/v1/agents" && r.Method == http.MethodGet {
		s.agentList(w)
		return true
	}
	if path == "/admin/v1/agents" && r.Method == http.MethodPut {
		s.agentGlobalMode(w, r)
		return true
	}
	if path == "/admin/v1/agents/profiles" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"data": s.agentStore.Profiles()})
		return true
	}
	if path == "/admin/v1/agents/profiles" && r.Method == http.MethodPut {
		var req struct {
			Name   string            `json:"name"`
			Agents map[string]string `json:"agents"`
		}
		if !decodeJSON(w, r, &req) {
			return true
		}
		p, err := s.agentStore.SaveProfile(req.Name, req.Agents)
		if err != nil {
			apiError(w, 400, "invalid_request", err.Error())
			return true
		}
		writeJSON(w, 200, map[string]any{"ok": true, "profile": p})
		return true
	}
	if !strings.HasPrefix(path, "/admin/v1/agents/") {
		apiError(w, 404, "not_found", "admin endpoint not found")
		return true
	}
	parts := strings.Split(strings.TrimPrefix(path, "/admin/v1/agents/"), "/")
	if len(parts) == 0 {
		apiError(w, 404, "not_found", "admin endpoint not found")
		return true
	}
	// Profile sub-routes: profiles/{name}/apply (POST) and profiles/{name} (DELETE).
	if parts[0] == "profiles" {
		if len(parts) == 3 && parts[2] == "apply" && r.Method == http.MethodPost {
			s.agentProfileApply(w, r, parts[1])
			return true
		}
		if len(parts) == 2 && r.Method == http.MethodDelete {
			if err := s.agentStore.DeleteProfile(parts[1]); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					apiError(w, 404, "profile_not_found", "no such profile")
					return true
				}
				apiError(w, 400, "invalid_request", err.Error())
				return true
			}
			writeJSON(w, 200, map[string]bool{"ok": true})
			return true
		}
		apiError(w, 404, "not_found", "admin endpoint not found")
		return true
	}
	if len(parts) > 2 {
		apiError(w, 404, "not_found", "admin endpoint not found")
		return true
	}
	id, action := parts[0], ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodDelete:
		if err := s.agentStore.Remove(id); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				apiError(w, 404, "agent_not_found", "no binding for this agent")
				return true
			}
			apiError(w, 400, "invalid_request", err.Error())
			return true
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return true
	case action == "bind" && r.Method == http.MethodPost:
		s.agentBind(w, r, id)
	case action == "restore" && r.Method == http.MethodPost:
		s.agentRestore(w, r, id)
	case action == "check" && r.Method == http.MethodPost:
		s.agentCheck(w, r, id)
	case action == "snippet" && r.Method == http.MethodGet:
		s.agentSnippet(w, r, id)
	case action == "version" && r.Method == http.MethodPost:
		// Serve from the one-hour cache when fresh; otherwise probe the CLI.
		rec := s.cachedAgentVersion(id, time.Hour)
		if rec == nil {
			rec = s.probeAgentVersion(r.Context(), id)
		}
		if rec == nil {
			apiError(w, 404, "no_probe", "该 Agent 没有版本探测命令")
			return true
		}
		writeJSON(w, 200, map[string]any{"ok": true, "version": rec})
		return true
	default:
		apiError(w, 404, "not_found", "admin endpoint not found")
	}
	return true
}

// agentProfileApply applies (or previews) every binding inside a profile.
// Preflight runs for all agents before any file is written, so a drifted or
// unbuildable agent aborts the whole switch without a half-applied config.
func (s *Server) agentProfileApply(w http.ResponseWriter, r *http.Request, name string) {
	var req struct {
		Preview bool `json:"preview"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	p := s.agentStore.Profile(name)
	if p == nil {
		apiError(w, 404, "profile_not_found", "no such profile")
		return
	}
	opts := agents.ApplyOptions{BaseURL: s.Config.Agents.GatewayURL, GatewayKey: s.Config.GatewayKey}
	ids := make([]string, 0, len(p.Agents))
	for id := range p.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	results := []map[string]any{}
	type pending struct {
		id    string
		b     *agents.Binding
		agent agents.Agent
	}
	var toApply []pending
	for _, id := range ids {
		agent, err := agents.Lookup(id)
		if err != nil {
			results = append(results, map[string]any{"agent": id, "ok": false, "error": err.Error()})
			continue
		}
		b := s.agentStore.Binding(id)
		if b == nil {
			b = &agents.Binding{AgentID: id, Mode: s.agentWriteMode(), Status: agents.StatusUnbound}
		}
		b.Model = p.Agents[id]
		if id == "dsh" && (b.Mode == "" || b.Mode == agents.ModeAuto) && b.TargetFile == "" {
			b.Mode = agents.ModeSnippet
		}
		if b.TargetFile == "" {
			b.TargetFile = agents.DefaultTarget(id)
		}
		if req.Preview {
			content, snippet, err := s.agentStore.BuildContent(agent, *b, opts)
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			results = append(results, map[string]any{
				"agent": id, "ok": err == nil, "mode": b.Mode,
				"content": string(content), "snippet": snippet, "target": b.TargetFile,
				"error": errMsg,
			})
			continue
		}
		status, note := s.agentStore.CurrentStatus(*b)
		if status == agents.StatusDrift {
			apiError(w, 400, "apply_failed", fmt.Sprintf("Agent %s 配置漂移：%s（请先在 Agents 页还原再应用方案）", id, note))
			return
		}
		if b.Mode == agents.ModeAuto {
			if _, _, err := s.agentStore.BuildContent(agent, *b, opts); err != nil {
				apiError(w, 400, "apply_failed", fmt.Sprintf("Agent %s 无法生成配置：%s", id, err.Error()))
				return
			}
		}
		toApply = append(toApply, pending{id: id, b: b, agent: agent})
	}
	applied := []string{}
	for _, pn := range toApply {
		content, snippet, res, _, err := s.agentStore.PersistApply(pn.agent, pn.b, opts)
		if err != nil {
			// Best effort: undo what was already written in this switch and
			// persist the restored binding state so status reflects reality.
			for _, id := range applied {
				if rb := s.agentStore.Binding(id); rb != nil && rb.BackupID != "" {
					if err := s.agentStore.Restore(rb); err == nil {
						_, _ = s.agentStore.Update(id, func(b *agents.Binding) {
							b.FileDigest = ""
							b.BackupID = ""
							b.Status = agents.StatusUnbound
							b.LastError = ""
						})
					}
				}
			}
			apiError(w, 400, "apply_failed", fmt.Sprintf("Agent %s 写入失败：%s（其余已切换的 Agent 已尽量还原）", pn.id, err.Error()))
			return
		}
		applied = append(applied, pn.id)
		results = append(results, map[string]any{"agent": pn.id, "ok": true, "wrote": res != nil, "backed_up": res != nil && res.BackedUp, "content": string(content), "snippet": snippet, "mode": pn.b.Mode})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "profile": name, "results": results})
}

func (s *Server) agentList(w http.ResponseWriter) {
	if !s.agentEnabled() {
		writeJSON(w, 200, map[string]any{"enabled": false, "write_mode": s.agentWriteMode(), "data": []any{}})
		return
	}
	rows := make([]agentOverview, 0, len(agents.Catalog()))
	for _, a := range agents.Catalog() {
		b := s.agentStore.Binding(a.ID)
		row := agentOverview{ID: a.ID, Name: a.Name, Hint: a.Hint, Requires: a.RequiresBridge}
		if b != nil {
			row.Binding = b
			status, note := s.agentStore.CurrentStatus(*b)
			row.Status = status
			row.DriftNote = note
			row.TargetFile = b.TargetFile
			row.HasBackup = b.BackupID != ""
			if status == agents.StatusUnbound {
				row.Status = "unbound"
			}
		} else {
			row.Status = "unbound"
		}
		if target := agents.DefaultTarget(a.ID); target != "" {
			if ok, err := agents.HasFile(resolveMaybe(target)); err == nil && ok {
				row.Installed = true
			}
			if row.TargetFile == "" {
				row.TargetFile = target
			}
		}
		// A snippet is always available; the UI shows it for snippet mode and
		// for agents that cannot be auto-written (dsh).
		if b != nil && b.Model != "" {
			if snip, err := agents.Snippet(a.ID, s.Config.Agents.GatewayURL, s.Config.GatewayKey, b.Model); err == nil {
				row.SnippetText = snip
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, 200, map[string]any{"enabled": true, "write_mode": s.agentWriteMode(), "gateway_url": s.Config.Agents.GatewayURL, "profiles": s.agentStore.Profiles(), "data": rows})
}

// resolveMaybe resolves a display path but tolerates resolve errors (the list
// endpoint must not fail because a home dir is unavailable).
func resolveMaybe(target string) string {
	t, err := agents.Resolve(target)
	if err != nil {
		return target
	}
	return t
}

type agentBindRequest struct {
	Model      string `json:"model"`
	Provider   string `json:"provider,omitempty"`
	Mode       string `json:"mode,omitempty"`
	TargetFile string `json:"target_file,omitempty"`
	Preview    bool   `json:"preview,omitempty"` // dry run: return content, do not write
}

func (s *Server) agentBind(w http.ResponseWriter, r *http.Request, id string) {
	var req agentBindRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	agent, err := agents.Lookup(id)
	if err != nil {
		apiError(w, 404, "unknown_agent", err.Error())
		return
	}
	if req.Model == "" {
		apiError(w, 400, "invalid_request", "请选择模型")
		return
	}
	if req.Mode == "" {
		req.Mode = s.agentWriteMode()
	}
	if req.Mode != agents.ModeAuto && req.Mode != agents.ModeSnippet {
		apiError(w, 400, "invalid_request", "写入模式必须是 auto 或 snippet")
		return
	}
	if req.Mode == agents.ModeAuto && agent.ID == "dsh" && req.TargetFile == "" {
		// DSH is env-var based; without an explicit target file there is
		// nothing to write, so fall back to snippet mode gracefully.
		req.Mode = agents.ModeSnippet
	}
	opts := agents.ApplyOptions{BaseURL: s.Config.Agents.GatewayURL, GatewayKey: s.Config.GatewayKey}
	if req.Preview {
		// Dry run: build the content from a temporary binding without
		// persisting anything, so "preview" never half-applies a model.
		target := req.TargetFile
		if target == "" {
			if b := s.agentStore.Binding(id); b != nil && b.TargetFile != "" {
				target = b.TargetFile
			} else {
				target = agents.DefaultTarget(id)
			}
		}
		tmp := &agents.Binding{AgentID: id, Model: req.Model, Provider: req.Provider, Mode: req.Mode, TargetFile: target}
		content, snippet, err := s.agentStore.BuildContent(agent, *tmp, opts)
		if err != nil {
			apiError(w, 400, "build_failed", err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"preview": true, "content": string(content), "snippet": snippet, "mode": req.Mode, "target": target})
		return
	}
	b, err := s.agentStore.Update(id, func(b *agents.Binding) {
		b.Model = req.Model
		b.Provider = req.Provider
		b.Mode = req.Mode
		if req.TargetFile != "" {
			b.TargetFile = req.TargetFile
		} else if b.TargetFile == "" {
			b.TargetFile = agents.DefaultTarget(id)
		}
		b.Status = agents.StatusUnbound
	})
	if err != nil {
		apiError(w, 400, "invalid_request", err.Error())
		return
	}
	content, snippet, res, b, err := s.agentStore.PersistApply(agent, b, opts)
	if err != nil {
		apiError(w, 400, "apply_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"binding":   b,
		"wrote":     res != nil,
		"backed_up": res != nil && res.BackedUp,
		"created":   res != nil && res.Created,
		"content":   string(content),
		"snippet":   snippet,
	})
}

func (s *Server) agentRestore(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := agents.Lookup(id); err != nil {
		apiError(w, 404, "unknown_agent", err.Error())
		return
	}
	b := s.agentStore.Binding(id)
	if b == nil {
		apiError(w, 404, "agent_not_found", "该 Agent 尚未接入")
		return
	}
	if err := s.agentStore.Restore(b); err != nil {
		apiError(w, 400, "restore_failed", err.Error())
		return
	}
	if _, err := s.agentStore.Update(id, func(b *agents.Binding) {
		b.FileDigest = ""
		b.BackupID = ""
		b.Status = agents.StatusUnbound
		b.LastError = ""
	}); err != nil {
		apiError(w, 500, "persistence_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "binding": s.agentStore.Binding(id)})
}

func (s *Server) agentSnippet(w http.ResponseWriter, r *http.Request, id string) {
	agent, err := agents.Lookup(id)
	if err != nil {
		apiError(w, 404, "unknown_agent", err.Error())
		return
	}
	b := s.agentStore.Binding(id)
	model := ""
	if b != nil {
		model = b.Model
	}
	if model == "" {
		apiError(w, 400, "no_model", "请先为该 Agent 选择模型")
		return
	}
	snippet, err := agents.Snippet(agent.ID, s.Config.Agents.GatewayURL, s.Config.GatewayKey, model)
	if err != nil {
		apiError(w, 400, "snippet_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"snippet": snippet})
}

// defaultWriteMode returns the write mode with the auto fallback applied
// (configs constructed outside config.Load may leave it empty).
func defaultWriteMode(mode string) string {
	if mode == "" {
		return agents.ModeAuto
	}
	return mode
}

// agentWriteMode returns the current default write mode (mutex-guarded: the
// UI can switch it at runtime while requests read it).
func (s *Server) agentWriteMode() string {
	s.agentsModeMu.RLock()
	defer s.agentsModeMu.RUnlock()
	if s.agentsMode == "" {
		return agents.ModeAuto
	}
	return s.agentsMode
}

func (s *Server) setAgentWriteMode(mode string) {
	s.agentsModeMu.Lock()
	defer s.agentsModeMu.Unlock()
	s.agentsMode = mode
}

// agentGlobalMode lets the UI switch the default write mode for the process.
func (s *Server) agentGlobalMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WriteMode string `json:"write_mode"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.WriteMode != agents.ModeAuto && req.WriteMode != agents.ModeSnippet {
		apiError(w, 400, "invalid_request", "write_mode 必须是 auto 或 snippet")
		return
	}
	s.setAgentWriteMode(req.WriteMode)
	writeJSON(w, 200, map[string]string{"write_mode": req.WriteMode})
}

// agentCheck performs the connectivity self-check: verify the model is listed
// by the gateway, then issue one tiny non-streaming chat request through the
// gateway itself using the client gateway key. The check consumes a minimal
// number of tokens on a real account; the PRD allows exactly this.
func (s *Server) agentCheck(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := agents.Lookup(id); err != nil {
		apiError(w, 404, "unknown_agent", err.Error())
		return
	}
	var req struct {
		Model    string `json:"model"`
		MaxToken int    `json:"max_tokens"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	if req.Model == "" {
		b := s.agentStore.Binding(id)
		if b == nil || b.Model == "" {
			apiError(w, 400, "no_model", "请先选择模型再检查")
			return
		}
		req.Model = b.Model
	}
	if req.MaxToken <= 0 {
		req.MaxToken = 8
	}
	if req.MaxToken > 256 {
		req.MaxToken = 256
	}
	status, note := s.runAgentSelfCheck(r, req.Model, req.MaxToken)
	_, _ = s.agentStore.Update(id, func(b *agents.Binding) {
		b.LastCheck = time.Now()
		b.CheckOK = status == "ok"
		b.CheckNote = note
	})
	writeJSON(w, 200, map[string]any{"ok": status == "ok", "status": status, "note": note, "agent": id, "model": req.Model})
}

// runAgentSelfCheck is split out so tests can exercise it without an admin
// request round-trip.
func (s *Server) runAgentSelfCheck(r *http.Request, model string, maxToken int) (status, note string) {
	group := s.Config.DefaultGroup
	if group == "" {
		group = pool.DefaultGroup
	}
	models, ok := s.Pool.ModelList(s.Config.DefaultProvider, group, 3*time.Duration(s.Config.RefreshIntervalSeconds)*time.Second)
	known := false
	if ok {
		for _, m := range models {
			if m["id"] == model {
				known = true
				break
			}
		}
	}
	if !known {
		return "model_missing", fmt.Sprintf("模型 %s 未出现在网关模型目录中（账号检查完成后才会出现，请先“检查”账号）", model)
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "连通性自检（gateway agent check）"}},
		"max_tokens": maxToken,
		"stream":     false,
	})
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.agentSelfCheckURL()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "error", err.Error()
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.Config.GatewayKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Session-ID", "agent-self-check")
	started := time.Now()
	resp, err := s.client.Do(httpReq)
	elapsed := time.Since(started).Milliseconds()
	if err != nil {
		return "error", "无法连接网关：" + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		return "ok", fmt.Sprintf("连通性检查通过 · %d ms", elapsed)
	case http.StatusUnauthorized:
		return "error", "网关拒绝了客户端 Key，请检查 GATEWAY_API_KEY"
	case http.StatusBadRequest:
		return "model_missing", fmt.Sprintf("网关拒绝了该模型（HTTP 400）：%s", truncateMessage(b))
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return "error", fmt.Sprintf("网关暂时不可用（HTTP %d），可能正在等待账号冷却：%s", resp.StatusCode, truncateMessage(b))
	default:
		return "error", fmt.Sprintf("网关返回 HTTP %d：%s", resp.StatusCode, truncateMessage(b))
	}
}

func truncateMessage(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
