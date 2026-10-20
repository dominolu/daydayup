package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/pool"
	"daydayup/internal/provider/workbuddy"
)

// WorkBuddy browser OAuth login admin endpoints. The flow mirrors the
// workbuddy2api-hub panel: the gateway asks the upstream plugin auth endpoint
// for a one-time login state + authorization URL, the admin UI opens that URL
// in the user's browser, and the gateway polls until the browser login
// completes, then auto-extracts uid / access_token / refresh_token / expiry,
// resolves the nickname and persists the account. Credentials never pass
// through the browser or the client: the UI only ever sees the state string.

const (
	wbLoginWindow = time.Minute
	wbLoginBurst  = 6
)

// wbLoginRecord is an in-flight login state. Only the state, realm and start
// time live in memory until the poll completes.
type wbLoginRecord struct {
	Created  time.Time
	Realm    string
	Platform string
}

// wbLoginStamp tracks start attempts per remote IP for a light rate limit.
type wbLoginStamp struct {
	At time.Time
	N  int
}

// workbuddyOAuthProvider is the small provider surface the OAuth routes need.
type workbuddyOAuthProvider interface {
	StartAuth(ctx context.Context, client *http.Client, realm, platform string) (*workbuddy.AuthStart, error)
	PollAuth(ctx context.Context, client *http.Client, realm, state string) (*workbuddy.AuthResult, error)
}

// workbuddyLoginAdmin dispatches /admin/v1/workbuddy/login/* routes.
func (s *Server) workbuddyLoginAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/admin/v1/workbuddy/login") {
		return false
	}
	switch r.URL.Path {
	case "/admin/v1/workbuddy/login/start":
		if r.Method != http.MethodPost {
			apiError(w, 405, "method_not_allowed", "POST required")
			return true
		}
		s.workbuddyLoginStart(w, r)
	case "/admin/v1/workbuddy/login/poll":
		if r.Method != http.MethodPost {
			apiError(w, 405, "method_not_allowed", "POST required")
			return true
		}
		s.workbuddyLoginPoll(w, r)
	case "/admin/v1/workbuddy/login/cancel":
		if r.Method != http.MethodPost {
			apiError(w, 405, "method_not_allowed", "POST required")
			return true
		}
		s.workbuddyLoginCancel(w, r)
	default:
		apiError(w, 404, "not_found", "admin endpoint not found")
	}
	return true
}

// workbuddyLoginStart begins a browser OAuth login. It asks the upstream for
// a state + authorization URL and remembers the state so poll can recognize
// it. The auth URL itself is opened by the admin UI, never fetched here.
func (s *Server) workbuddyLoginStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Realm    string `json:"realm"`
		Platform string `json:"platform"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	realm := strings.TrimSpace(body.Realm)
	if realm == "" {
		realm = s.Config.DefaultRealm
	}
	if realm == "" {
		realm = "intl"
	}
	if realm != "intl" && realm != "cn" {
		apiError(w, 400, "invalid_realm", "realm must be intl or cn")
		return
	}
	platform := strings.TrimSpace(body.Platform)
	if platform == "" {
		platform = "CLI"
	}
	if len(platform) > 32 || strings.ContainsAny(platform, "\r\n\x00") {
		apiError(w, 400, "invalid_request", "platform is invalid")
		return
	}
	if !s.wbLoginAllowed(remoteIP(r)) {
		apiError(w, 429, "rate_limited", "登录发起过于频繁，请稍后再试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	prov := s.workbuddyOAuth()
	if prov == nil {
		apiError(w, 500, "provider_unavailable", "workbuddy provider is not registered")
		return
	}
	start, err := prov.StartAuth(ctx, s.client, realm, platform)
	if err != nil {
		apiError(w, 502, "login_start_failed", "无法开始授权登录："+err.Error())
		return
	}
	s.wbLoginMu.Lock()
	s.wbLogins[start.State] = wbLoginRecord{Created: time.Now(), Realm: realm, Platform: platform}
	s.wbLoginMu.Unlock()
	writeJSON(w, 200, map[string]any{"state": start.State, "auth_url": start.AuthURL, "realm": start.Realm, "platform": start.Platform})
}

// workbuddyLoginPoll advances an in-flight login. Pending keeps the UI
// waiting; ok carries the freshly persisted account (uid/tokens extracted
// upstream and saved by the gateway).
func (s *Server) workbuddyLoginPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	state := strings.TrimSpace(body.State)
	if state == "" || len(state) > 128 {
		apiError(w, 400, "invalid_request", "state is required")
		return
	}
	s.wbLoginMu.Lock()
	rec, known := s.wbLogins[state]
	expired := known && time.Since(rec.Created) > workbuddy.LoginTTL
	if known && expired {
		delete(s.wbLogins, state)
	}
	s.wbLoginMu.Unlock()
	if !known {
		writeJSON(w, 200, map[string]string{"status": "unknown", "message": "state not recognised - start the login again"})
		return
	}
	if expired {
		writeJSON(w, 200, map[string]string{"status": "expired", "message": "登录窗口已过期，请重新开始授权"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	prov := s.workbuddyOAuth()
	if prov == nil {
		apiError(w, 500, "provider_unavailable", "workbuddy provider is not registered")
		return
	}
	result, err := prov.PollAuth(ctx, s.client, rec.Realm, state)
	if err != nil {
		apiError(w, 502, "login_poll_failed", "授权轮询失败："+err.Error())
		return
	}
	if result.Status != "ok" {
		writeJSON(w, 200, map[string]any{"status": result.Status, "message": result.Message})
		return
	}
	acc := result.Account
	if acc == nil || acc.AccessToken == "" || acc.UID == "" {
		apiError(w, 502, "login_incomplete", "授权结果缺少账号凭证")
		return
	}
	id, updated, err := s.persistOAuthAccount(acc)
	if err != nil {
		apiError(w, 409, "login_save_failed", "授权成功但账号保存失败："+err.Error())
		return
	}
	s.wbLoginMu.Lock()
	delete(s.wbLogins, state)
	s.wbLoginMu.Unlock()
	writeJSON(w, 200, map[string]any{"status": "ok", "message": "登录成功", "account_id": id, "updated": updated, "account": s.accountStates(id)})
}

// workbuddyLoginCancel forgets a login state the user gave up on.
func (s *Server) workbuddyLoginCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	state := strings.TrimSpace(body.State)
	if state == "" {
		apiError(w, 400, "invalid_request", "state is required")
		return
	}
	s.wbLoginMu.Lock()
	_, found := s.wbLogins[state]
	delete(s.wbLogins, state)
	s.wbLoginMu.Unlock()
	writeJSON(w, 200, map[string]bool{"cancelled": found})
}

// persistOAuthAccount stores the login result as a managed WorkBuddy account.
// The account id derives from the upstream uid so logging the same account in
// again rotates its tokens in place instead of creating a duplicate.
func (s *Server) persistOAuthAccount(acc *workbuddy.AuthAccount) (id string, updated bool, err error) {
	id = "wb-" + acc.UID
	if len(id) > 64 {
		id = id[:64]
	}
	name := strings.TrimSpace(acc.Nickname)
	if name == "" {
		name = acc.UID
	}
	a := config.Account{
		ID:           id,
		Name:         name,
		Provider:     "workbuddy",
		APIKey:       acc.AccessToken,
		RefreshToken: acc.RefreshToken,
		UID:          acc.UID,
		Realm:        acc.Realm,
		Domain:       acc.Domain,
		Product:      "desktop",
		MaxInFlight:  4,
	}
	if err = config.NormalizeAccount(&a); err != nil {
		return id, false, err
	}
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	create := true
	if _, findErr := s.Pool.Account(id); findErr == nil {
		create = false
	}
	if err = s.Pool.Upsert(a, create, s.accountStore.SaveAccount); err != nil {
		if errors.Is(err, pool.ErrDuplicate) {
			// Same credential already registered under a different id.
			return id, false, errors.New("该凭证已存在于其他账号")
		}
		if errors.Is(err, os.ErrNotExist) {
			// Account vanished between the lookup and the upsert; retry once.
			create = true
			err = s.Pool.Upsert(a, create, s.accountStore.SaveAccount)
		}
		if err != nil {
			return id, false, err
		}
	}
	if err = s.Pool.Save(s.Config.DBFile); err != nil {
		return id, !create, fmt.Errorf("账号已保存，但统计状态落盘失败: %w", err)
	}
	return id, !create, nil
}

// workbuddyOAuth returns the registered workbuddy provider as its OAuth
// surface, or nil when the provider is absent.
func (s *Server) workbuddyOAuth() workbuddyOAuthProvider {
	p, ok := s.providers.Get("workbuddy")
	if !ok {
		return nil
	}
	if oa, ok := p.(workbuddyOAuthProvider); ok {
		return oa
	}
	return nil
}

// wbLoginAllowed enforces a per-IP start budget: wbLoginBurst starts per
// wbLoginWindow. It also prunes stale stamps so the map stays bounded.
func (s *Server) wbLoginAllowed(ip string) bool {
	if ip == "" {
		ip = "unknown"
	}
	now := time.Now()
	s.wbLoginMu.Lock()
	defer s.wbLoginMu.Unlock()
	for k, v := range s.wbLoginIPs {
		if now.Sub(v.At) >= wbLoginWindow {
			delete(s.wbLoginIPs, k)
		}
	}
	stamp := s.wbLoginIPs[ip]
	if stamp.N >= wbLoginBurst {
		return false
	}
	stamp.N++
	stamp.At = now
	s.wbLoginIPs[ip] = stamp
	return true
}
