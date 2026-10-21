package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/pool"
)

func (s *Server) LoadAccounts() error {
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	accounts, err := s.accountStore.Load(s.Config.Accounts)
	if err != nil {
		return err
	}
	s.Pool = pool.New(accounts)
	return nil
}

type accountInput struct {
	ID           string    `json:"id"`
	Name         *string   `json:"name"`
	Provider     string    `json:"provider"`
	BaseURL      *string   `json:"base_url"`
	Group        *string   `json:"group"` // routing group; nil keeps the stored value
	APIKey       string    `json:"api_key"`
	MaxInFlight  *int      `json:"max_in_flight"`
	Models       *[]string `json:"models"`
	RefreshToken string    `json:"refresh_token"`
	UID          string    `json:"uid"`
	Realm        string    `json:"realm"`
	Product      string    `json:"product"`
	Domain       string    `json:"domain"`
	EnterpriseID string    `json:"enterprise_id"`
}

func (s *Server) accountStates(id string) any {
	for _, state := range s.Pool.Snapshot() {
		if state.ID == id {
			return state
		}
	}
	return nil
}
func mutationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pool.ErrBusy):
		apiError(w, 409, "account_busy", "账号仍有进行中的请求，请先停用并等待请求结束")
	case errors.Is(err, pool.ErrDuplicate):
		apiError(w, 409, "duplicate_account", "账号 ID 或相同源站的 API Key 已存在")
	case errors.Is(err, os.ErrNotExist):
		apiError(w, 404, "account_not_found", "账号不存在")
	default:
		apiError(w, 500, "account_store_failed", "账号未保存，请检查本地账号存储与加密密钥配置")
	}
}

// importTokensPayload is the paste-import format for WorkBuddy / CodeBuddy
// OAuth accounts. access_token/refresh_token/uid come from an account export
// or the desktop client's token store; the gateway never performs the OAuth
// dance itself.
type importTokensPayload struct {
	Accounts []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		UID          string `json:"uid"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Realm        string `json:"realm"`
		Product      string `json:"product"`
		Group        string `json:"group"`
		BaseURL      string `json:"base_url"`
		MaxInFlight  int    `json:"max_in_flight"`
	} `json:"accounts"`
}

func (s *Server) importTokens(w http.ResponseWriter, r *http.Request) {
	var payload importTokensPayload
	if !decodeJSON(w, r, &payload) {
		return
	}
	if len(payload.Accounts) == 0 {
		apiError(w, 400, "invalid_request", "accounts is required")
		return
	}
	if len(payload.Accounts) > 200 {
		apiError(w, 400, "invalid_request", "at most 200 accounts per import")
		return
	}
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	created := []map[string]string{}
	errors := []map[string]string{}
	for _, in := range payload.Accounts {
		a := config.Account{
			ID:           in.ID,
			Name:         strings.TrimSpace(in.Name),
			Provider:     "workbuddy",
			Group:        in.Group,
			BaseURL:      strings.TrimSpace(in.BaseURL),
			APIKey:       strings.TrimSpace(in.AccessToken),
			RefreshToken: strings.TrimSpace(in.RefreshToken),
			UID:          strings.TrimSpace(in.UID),
			Realm:        in.Realm,
			Product:      in.Product,
			MaxInFlight:  in.MaxInFlight,
		}
		if err := config.NormalizeAccount(&a); err != nil {
			errors = append(errors, map[string]string{"id": in.ID, "error": err.Error()})
			continue
		}
		if err := s.Pool.Upsert(a, true, s.accountStore.SaveAccount); err != nil {
			errors = append(errors, map[string]string{"id": in.ID, "error": err.Error()})
			continue
		}
		created = append(created, map[string]string{"id": a.ID, "name": a.Name})
	}
	if err := s.Pool.Save(s.Config.DBFile); err != nil {
		slog.Error("state checkpoint failed after token import", "error", err)
	}
	writeJSON(w, 200, map[string]any{"imported": len(created), "created": created, "errors": errors})
}

func (s *Server) accountAdmin(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/admin/v1/session-adapters" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"data": sessionAdapters()})
		return true
	}
	if r.URL.Path == "/admin/v1/info" && r.Method == http.MethodGet {
		name := s.Config.InstanceName
		if name == "" {
			name = "DayDayUp 网关"
		}
		writeJSON(w, 200, map[string]any{"instance_name": name, "version": "0.6.0", "session_storage": s.Sessions != nil, "session_titles": true, "pool_waiting": true, "rate_limit_diagnostics": true, "sse_completion": "terminal_event_after_successful_flush", "max_waiting_requests": cap(s.waitSlots), "request_timeout_seconds": s.Config.RequestTimeoutSeconds, "session_adapters": sessionAdapters(), "session_identity": "native_headers_tools_and_titles", "default_provider": s.Config.DefaultProvider, "default_group": s.Config.DefaultGroup, "default_realm": s.Config.DefaultRealm, "groups": s.Pool.Groups(), "guards": s.Config.Guards, "refresh_interval_seconds": s.Config.RefreshIntervalSeconds, "account_storage": "AES-256-GCM", "quota_api": "unsupported", "platforms": platformCatalog()})
		return true
	}
	if r.URL.Path == "/admin/v1/accounts/import" && r.Method == http.MethodPost {
		s.importTokens(w, r)
		return true
	}
	if s.workbuddyLoginAdmin(w, r) {
		return true
	}
	create := r.URL.Path == "/admin/v1/accounts" && r.Method == http.MethodPost
	var id, action string
	if strings.HasPrefix(r.URL.Path, "/admin/v1/accounts/") {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/v1/accounts/"), "/")
		if len(parts) == 1 {
			id = parts[0]
		} else if len(parts) == 2 {
			id, action = parts[0], parts[1]
		} else if len(parts) == 3 && parts[1] == "tasks" {
			// /admin/v1/accounts/{id}/tasks/{checkin|activity|daily-chat}
			id, action = parts[0], "tasks"
		}
	}
	if id != "" && action == "check" && r.Method == http.MethodPost {
		a, err := s.Pool.Account(id)
		if err != nil {
			mutationError(w, err)
			return true
		}
		s.checkAccount(r.Context(), a)
		writeJSON(w, 200, map[string]any{"data": s.accountStates(id)})
		return true
	}
	if id != "" && action == "tasks" {
		if ok, kind := s.adminWorkBuddyTaskRoute(w, r); ok {
			s.workbuddyTaskAdmin(w, id, kind)
			return true
		}
	}
	if id != "" && action == "" && r.Method == http.MethodDelete {
		s.accountsMu.Lock()
		err := s.Pool.Remove(id, s.accountStore.Delete)
		s.accountsMu.Unlock()
		if err != nil {
			mutationError(w, err)
			return true
		}
		if err = s.Pool.Save(s.Config.DBFile); err != nil {
			apiError(w, 500, "checkpoint_failed", "账号已移除，但统计状态保存失败")
			return true
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return true
	}
	if !create && !(id != "" && action == "" && r.Method == http.MethodPut) {
		return false
	}
	var input accountInput
	if !decodeJSON(w, r, &input) {
		return true
	}
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	a := config.Account{ID: input.ID, Provider: input.Provider, MaxInFlight: 4}
	if !create {
		var err error
		a, err = s.Pool.Account(id)
		if err != nil {
			mutationError(w, err)
			return true
		}
		if input.ID != "" && input.ID != id {
			apiError(w, 400, "immutable_id", "账号 ID 创建后不可修改")
			return true
		}
		if input.Provider != "" && input.Provider != a.Provider {
			apiError(w, 400, "immutable_provider", "平台创建后不可修改")
			return true
		}
	}
	if input.Name != nil {
		a.Name = strings.TrimSpace(*input.Name)
	}
	if input.BaseURL != nil {
		a.BaseURL = strings.TrimSpace(*input.BaseURL)
	}
	if input.MaxInFlight != nil {
		if *input.MaxInFlight < 1 || *input.MaxInFlight > 1000 {
			apiError(w, 400, "invalid_account", "最大并发必须介于 1 和 1000")
			return true
		}
		a.MaxInFlight = *input.MaxInFlight
	}
	if input.Models != nil {
		a.Models = *input.Models
	}
	if input.APIKey != "" {
		a.APIKey = strings.TrimSpace(input.APIKey)
		a.APIKeyEnv = ""
	}
	// WorkBuddy fields: empty values on edit keep the current stored value.
	if input.RefreshToken != "" {
		a.RefreshToken = strings.TrimSpace(input.RefreshToken)
	}
	if input.UID != "" {
		a.UID = strings.TrimSpace(input.UID)
	}
	if input.Realm != "" {
		a.Realm = input.Realm
	}
	if input.Product != "" {
		a.Product = input.Product
	}
	if input.Domain != "" {
		a.Domain = strings.TrimSpace(input.Domain)
	}
	if input.EnterpriseID != "" {
		a.EnterpriseID = strings.TrimSpace(input.EnterpriseID)
	}
	if input.Group != nil {
		a.Group = strings.TrimSpace(*input.Group)
	}
	if err := config.NormalizeAccount(&a); err != nil {
		apiError(w, 400, "invalid_account", err.Error())
		return true
	}
	if err := s.Pool.Upsert(a, create, s.accountStore.SaveAccount); err != nil {
		mutationError(w, err)
		return true
	}
	status := http.StatusOK
	if create {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"data": s.accountStates(a.ID), "saved_at": time.Now().UTC()})
	return true
}
