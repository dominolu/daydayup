package gateway

// Group and routing-policy administration (web console):
//   - GET    /admin/v1/groups                     list groups with account/model/traffic stats
//   - PUT    /admin/v1/groups/default             set the default routing group (config write-back)
//   - PUT    /admin/v1/groups/{name}              rename a group {rename}
//   - DELETE /admin/v1/groups/{name}              dissolve a group, moving accounts to {move_to|default}
//   - GET    /admin/v1/middleware                 model_map aliases + model_rename + routing strategy
//   - PUT    /admin/v1/middleware/model_map       hot-reload model aliases
//   - PUT    /admin/v1/middleware/model_rename    hot-reload per-platform model renames
//   - PUT    /admin/v1/routing                    switch round_robin / smart
//
// Everything related to the running pool applies in memory first (the gate
// that makes mistakes cheap), then the section is written back to the config
// file the same way redaction rules do, so a restart keeps the change.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"safuno/internal/config"
	"safuno/internal/pool"
)

// configWriteMu serializes atomic config-file write-backs from the admin UI.
var configWriteMu sync.Mutex

// writeConfigField atomically replaces one top-level section of the config
// file (SourcePath) with value. Raw, unresolvable fields stay untouched: only
// the selected key is rewritten, exactly like saveRedactionRules.
func (s *Server) writeConfigField(key string, value any) error {
	if s.Config.SourcePath == "" {
		return nil
	}
	path := s.Config.SourcePath
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	doc[key], err = json.Marshal(value)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".config-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(encoded); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (s *Server) groupsAdmin(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if path == "/admin/v1/groups" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"data": s.Pool.Groups(), "default_group": pool.DefaultGroup, "config_default_group": s.Config.DefaultGroup})
		return true
	}
	if path == "/admin/v1/groups/default" && r.Method == http.MethodPut {
		var body struct {
			Name string `json:"name"`
		}
		if !decodeJSON(w, r, &body) {
			return true
		}
		group, err := config.NormalizeGroup(body.Name)
		if err != nil {
			apiError(w, 400, "invalid_group", err.Error())
			return true
		}
		if err := s.writeConfigField("default_group", group); err != nil {
			apiError(w, 500, "config_save_failed", "default group could not be saved; running default was retained")
			return true
		}
		s.Config.DefaultGroup = group
		writeJSON(w, 200, map[string]any{"ok": true, "default_group": group})
		return true
	}
	if path == "/admin/v1/groups/default" && r.Method == http.MethodDelete {
		apiError(w, 400, "default_group_locked", "不能删除默认分组：请先把另一个分组设为默认，或直接重命名本组")
		return true
	}
	// Per-group routing strategy: PUT /admin/v1/groups/{name}/routing
	// {"strategy":"round_robin|smart"} pins one group to its own mode; groups
	// without an override keep following the global "routing" field.
	if strings.HasSuffix(path, "/routing") && r.Method == http.MethodPut {
		base := strings.TrimSuffix(path, "/routing")
		name := strings.TrimPrefix(base, "/admin/v1/groups/")
		if name == "" {
			return false
		}
		group, err := config.NormalizeGroup(name)
		if err != nil {
			apiError(w, 400, "invalid_group", err.Error())
			return true
		}
		var body struct {
			Strategy string `json:"strategy"`
		}
		if !decodeJSON(w, r, &body) {
			return true
		}
		if body.Strategy != pool.RoutingRoundRobin && body.Strategy != pool.RoutingSmart {
			apiError(w, 400, "invalid_routing", "routing strategy must be round_robin or smart")
			return true
		}
		next := map[string]string{}
		for k, v := range s.Config.GroupRouting {
			next[k] = v
		}
		next[group] = body.Strategy
		if err := s.writeConfigField("group_routing", next); err != nil {
			apiError(w, 500, "config_save_failed", "调度策略未能保存；运行中的策略保留")
			return true
		}
		s.Config.GroupRouting = next
		s.Pool.SetGroupRoutingMap(next)
		writeJSON(w, 200, map[string]any{"ok": true, "group": group, "routing": body.Strategy})
		return true
	}
	if !strings.HasPrefix(path, "/admin/v1/groups/") || path == "/admin/v1/groups/default" {
		return false
	}
	name := strings.TrimPrefix(path, "/admin/v1/groups/")
	parts := strings.Split(name, "/")
	if len(parts) != 1 {
		return false
	}
	group, err := config.NormalizeGroup(parts[0])
	if err != nil {
		apiError(w, 400, "invalid_group", err.Error())
		return true
	}
	switch r.Method {
	case http.MethodPut: // rename
		var body struct {
			Rename string `json:"rename"`
		}
		if !decodeJSON(w, r, &body) {
			return true
		}
		next, err := config.NormalizeGroup(body.Rename)
		if err != nil {
			apiError(w, 400, "invalid_group", err.Error())
			return true
		}
		if next == group {
			writeJSON(w, 200, map[string]any{"ok": true, "renamed": group, "moved": 0})
			return true
		}
		if err := s.Pool.MoveGroup(group, next, true, s.accountStore.SaveAccount); err != nil {
			if errors.Is(err, pool.ErrEmptyGroup) {
				apiError(w, 404, "group_not_found", "分组下没有账号，无法重命名（为空的分组随账号消失）")
			} else {
				apiError(w, 500, "group_rename_failed", "分组重命名失败，请检查账号存储")
			}
			return true
		}
		if err := s.Pool.Save(s.Config.DBFile); err != nil {
			apiError(w, 500, "checkpoint_failed", "分组已重命名，但统计状态保存失败")
			return true
		}
		defaultMoved := false
		if s.Config.DefaultGroup == group {
			if err := s.writeConfigField("default_group", next); err != nil {
				apiError(w, 500, "config_save_failed", "分组已重命名，但默认分组配置写入失败")
				return true
			}
			s.Config.DefaultGroup = next
			defaultMoved = true
		}
		// Carry the per-group routing override to the new name.
		if v, ok := s.Config.GroupRouting[group]; ok {
			nextMap := map[string]string{}
			for k, val := range s.Config.GroupRouting {
				nextMap[k] = val
			}
			delete(nextMap, group)
			nextMap[next] = v
			if err := s.writeConfigField("group_routing", nextMap); err != nil {
				apiError(w, 500, "config_save_failed", "分组已重命名，但分组调度策略写入失败")
				return true
			}
			s.Config.GroupRouting = nextMap
			s.Pool.SetGroupRoutingMap(nextMap)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "renamed": group, "to": next, "default_group": defaultMoved})
		return true
	case http.MethodDelete: // dissolve: accounts move to move_to (default "default")
		var body struct {
			MoveTo string `json:"move_to"`
		}
		// An absent/empty body means "move to the default group"; only a body
		// that is actually present and malformed is rejected (decodeJSON has
		// already written the 400 response in that case).
		if r.Body != nil && r.ContentLength != 0 {
			if !decodeJSON(w, r, &body) {
				return true
			}
		}
		target, err := config.NormalizeGroup(body.MoveTo)
		if err != nil || target == "" {
			target = pool.DefaultGroup
		}
		if s.Config.DefaultGroup == group {
			apiError(w, 400, "default_group_locked", "不能删除默认分组：请先把另一个分组设为默认，或直接重命名本组")
			return true
		}
		if err := s.Pool.MoveGroup(group, target, false, s.accountStore.SaveAccount); err != nil {
			apiError(w, 500, "group_delete_failed", "分组解散失败，请检查账号存储")
			return true
		}
		if err := s.Pool.Save(s.Config.DBFile); err != nil {
			apiError(w, 500, "checkpoint_failed", "分组已解散，但统计状态保存失败")
			return true
		}
		// A dissolved group loses its per-group routing override.
		if _, ok := s.Config.GroupRouting[group]; ok {
			nextMap := map[string]string{}
			for k, val := range s.Config.GroupRouting {
				if k != group {
					nextMap[k] = val
				}
			}
			if err := s.writeConfigField("group_routing", nextMap); err != nil {
				apiError(w, 500, "config_save_failed", "分组已解散，但分组调度策略配置未能清理")
				return true
			}
			s.Config.GroupRouting = nextMap
			s.Pool.ClearGroupRouting(group)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "deleted": group, "moved_to": target})
		return true
	}
	return false
}

func (s *Server) middlewareAdmin(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if path == "/admin/v1/middleware" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{
			"model_map":    s.Config.Middleware.ModelMap,
			"model_rename": s.Config.ModelRename,
			"routing":      s.Config.Routing,
		})
		return true
	}
	if path == "/admin/v1/middleware/model_map" && r.Method == http.MethodPut {
		var body struct {
			ModelMap map[string]string `json:"model_map"`
		}
		if !decodeJSON(w, r, &body) {
			return true
		}
		next := map[string]string{}
		for client, upstream := range body.ModelMap {
			client, upstream = strings.TrimSpace(client), strings.TrimSpace(upstream)
			if client == "" || upstream == "" {
				apiError(w, 400, "invalid_model_map", "模型别名与目标模型都不能为空")
				return true
			}
			next[client] = upstream
		}
		if len(next) == 0 && len(body.ModelMap) != 0 {
			// map was sent but every entry was empty: reject rather than wipe
			apiError(w, 400, "invalid_model_map", "模型别名不能为空")
			return true
		}
		s.Config.Middleware.ModelMap = next
		if err := s.writeConfigField("middleware", s.Config.Middleware); err != nil {
			apiError(w, 500, "config_save_failed", "模型别名未能保存；运行中的别名保留")
			return true
		}
		writeJSON(w, 200, map[string]any{"ok": true, "model_map": next})
		return true
	}
	if path == "/admin/v1/middleware/model_rename" && r.Method == http.MethodPut {
		var body struct {
			ModelRename map[string]map[string]string `json:"model_rename"`
		}
		if !decodeJSON(w, r, &body) {
			return true
		}
		next, err := config.NormalizeModelRename(body.ModelRename)
		if err != nil {
			apiError(w, 400, "invalid_model_rename", err.Error())
			return true
		}
		if len(next) == 0 && len(body.ModelRename) != 0 {
			apiError(w, 400, "invalid_model_rename", "模型改名不能全部为空")
			return true
		}
		s.Config.ModelRename = next
		if err := s.writeConfigField("model_rename", next); err != nil {
			apiError(w, 500, "config_save_failed", "模型改名未能保存；运行中的配置保留")
			return true
		}
		s.Pool.SetModelRename(next)
		writeJSON(w, 200, map[string]any{"ok": true, "model_rename": next})
		return true
	}
	if path == "/admin/v1/routing" && r.Method == http.MethodPut {
		var body struct {
			Strategy string `json:"strategy"`
		}
		if !decodeJSON(w, r, &body) {
			return true
		}
		if body.Strategy != pool.RoutingRoundRobin && body.Strategy != pool.RoutingSmart {
			apiError(w, 400, "invalid_routing", "routing strategy must be round_robin or smart")
			return true
		}
		if err := s.writeConfigField("routing", body.Strategy); err != nil {
			apiError(w, 500, "config_save_failed", "调度策略未能保存；运行中的策略保留")
			return true
		}
		s.Config.Routing = body.Strategy
		s.Pool.SetRouting(body.Strategy)
		writeJSON(w, 200, map[string]any{"ok": true, "routing": body.Strategy})
		return true
	}
	return false
}
