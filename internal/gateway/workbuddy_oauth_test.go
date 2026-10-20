package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"daydayup/internal/accounts"
	"daydayup/internal/config"
	"daydayup/internal/provider/workbuddy"
)

// oauthTestToken builds a fake JWT carrying uid/exp so the mock upstream can
// hand back credentials that exercise the gateway's extraction path.
func oauthTestToken(uid string) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"sub": uid, "exp": float64(time.Now().Add(2 * time.Hour).Unix())})
	return head + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// newOAuthGateway wires a gateway whose OAuth endpoints talk to the mock
// upstream, with a real encrypted account store under dir.
func newOAuthGateway(t *testing.T, dir string) *Server {
	t.Helper()
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, DBFile: dir + "/gateway.db", DefaultRealm: "intl"}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return s
}

func oauthAdminCall(s *Server, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/admin/v1"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// oauthMockUpstream serves the plugin auth endpoints and flips from pending to
// success after n polls, so the test can observe both states.
func oauthMockUpstream(t *testing.T, pollsBeforeSuccess int) *httptest.Server {
	t.Helper()
	var polls atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"state":"state-abc","authUrl":"https://www.workbuddy.ai/login?platform=CLI&state=state-abc"}}`)
		case "/v2/plugin/auth/token":
			w.Header().Set("Content-Type", "application/json")
			if polls.Add(1) <= int64(pollsBeforeSuccess) {
				_, _ = io.WriteString(w, `{"code":11217,"msg":"11217:login ing..."}`)
				return
			}
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"accessToken":%q,"refreshToken":"refresh-1","expiresAt":%d,"domain":"www.workbuddy.ai"}}`, oauthTestToken("uid-123"), time.Now().Add(3*time.Hour).Unix())
		case "/v2/plugin/login/account":
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
				t.Errorf("profile authorization header %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":{"nickname":"测试主号"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestWorkBuddyOAuthLoginLifecycle(t *testing.T) {
	up := oauthMockUpstream(t, 1)
	defer up.Close()
	restore := workbuddy.OverrideUpstream("intl", up.URL)
	defer restore()
	dir := t.TempDir()
	s := newOAuthGateway(t, dir)

	// 1. start
	start := oauthAdminCall(s, "/workbuddy/login/start", `{"realm":"intl","platform":"CLI"}`)
	if start.Code != 200 {
		t.Fatalf("start: %d %s", start.Code, start.Body)
	}
	var started struct {
		State   string `json:"state"`
		AuthURL string `json:"auth_url"`
		Realm   string `json:"realm"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.State != "state-abc" || started.AuthURL == "" || started.Realm != "intl" {
		t.Fatalf("started: %+v", started)
	}

	// 2. first poll stays pending
	pending := oauthAdminCall(s, "/workbuddy/login/poll", `{"state":"state-abc"}`)
	if pending.Code != 200 || !strings.Contains(pending.Body.String(), `"status":"pending"`) {
		t.Fatalf("pending: %d %s", pending.Code, pending.Body)
	}

	// 3. second poll completes and the gateway persists the account
	done := oauthAdminCall(s, "/workbuddy/login/poll", `{"state":"state-abc"}`)
	if done.Code != 200 {
		t.Fatalf("poll done: %d %s", done.Code, done.Body)
	}
	var result struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
		Updated   bool   `json:"updated"`
	}
	if err := json.Unmarshal(done.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || result.Updated {
		t.Fatalf("poll result: %+v", result)
	}
	acc, err := s.Pool.Account(result.AccountID)
	if err != nil {
		t.Fatalf("pool account: %v", err)
	}
	if acc.UID != "uid-123" || acc.RefreshToken != "refresh-1" || acc.Realm != "intl" || acc.Product != "desktop" {
		t.Fatalf("account: %+v", acc)
	}
	if !strings.HasPrefix(acc.APIKey, "eyJ") {
		t.Fatalf("access token not extracted: %q", acc.APIKey)
	}

	// 4. tokens must land in the encrypted account store
	st, storeErr := accounts.New(dir + "/gateway.db")
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	loaded, err := st.Load(s.Config.Accounts)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("store: %v %v", loaded, err)
	}
	if loaded[0].APIKey != acc.APIKey || loaded[0].RefreshToken != "refresh-1" || loaded[0].UID != "uid-123" {
		t.Fatalf("store record: %+v", loaded[0])
	}

	// 5. the state is single-use: a further poll is unknown
	again := oauthAdminCall(s, "/workbuddy/login/poll", `{"state":"state-abc"}`)
	if !strings.Contains(again.Body.String(), `"status":"unknown"`) {
		t.Fatalf("reused state: %d %s", again.Code, again.Body)
	}
}

func TestWorkBuddyOAuthReloginUpdatesTokens(t *testing.T) {
	up := oauthMockUpstream(t, 0)
	defer up.Close()
	restore := workbuddy.OverrideUpstream("intl", up.URL)
	defer restore()
	dir := t.TempDir()
	s := newOAuthGateway(t, dir)

	first := oauthAdminCall(s, "/workbuddy/login/start", `{"realm":"intl"}`)
	if first.Code != 200 {
		t.Fatalf("first start: %d", first.Code)
	}
	var started struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &started)
	poll := oauthAdminCall(s, "/workbuddy/login/poll", fmt.Sprintf(`{"state":%q}`, started.State))
	var result struct {
		AccountID string `json:"account_id"`
		Updated   bool   `json:"updated"`
	}
	_ = json.Unmarshal(poll.Body.Bytes(), &result)
	if result.AccountID == "" {
		t.Fatalf("first poll: %s", poll.Body)
	}

	// Same uid logs in again: tokens rotate in place, no duplicate.
	second := oauthAdminCall(s, "/workbuddy/login/start", `{"realm":"intl"}`)
	_ = json.Unmarshal(second.Body.Bytes(), &started)
	poll2 := oauthAdminCall(s, "/workbuddy/login/poll", fmt.Sprintf(`{"state":%q}`, started.State))
	var result2 struct {
		AccountID string `json:"account_id"`
		Updated   bool   `json:"updated"`
	}
	_ = json.Unmarshal(poll2.Body.Bytes(), &result2)
	if result2.AccountID != result.AccountID || !result2.Updated {
		t.Fatalf("relogin: %+v want same id updated", result2)
	}
	if n := len(s.Pool.Accounts()); n != 1 {
		t.Fatalf("relogin must not duplicate: %d accounts", n)
	}
}

func TestWorkBuddyOAuthStartRateLimit(t *testing.T) {
	up := oauthMockUpstream(t, 0)
	defer up.Close()
	restore := workbuddy.OverrideUpstream("intl", up.URL)
	defer restore()
	s := newOAuthGateway(t, t.TempDir())
	var ok int
	for i := 0; i < wbLoginBurst+2; i++ {
		w := oauthAdminCall(s, "/workbuddy/login/start", `{"realm":"intl"}`)
		if w.Code == 200 {
			ok++
		} else if w.Code == 429 {
			t.Logf("start %d rate limited as expected", i+1)
		} else {
			t.Fatalf("start %d: %d %s", i+1, w.Code, w.Body)
		}
	}
	if ok != wbLoginBurst {
		t.Fatalf("allowed %d starts, want %d", ok, wbLoginBurst)
	}
}

func TestWorkBuddyOAuthCancel(t *testing.T) {
	up := oauthMockUpstream(t, 0)
	defer up.Close()
	restore := workbuddy.OverrideUpstream("intl", up.URL)
	defer restore()
	s := newOAuthGateway(t, t.TempDir())

	start := oauthAdminCall(s, "/workbuddy/login/start", `{"realm":"intl"}`)
	var started struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(start.Body.Bytes(), &started)

	cancel := oauthAdminCall(s, "/workbuddy/login/cancel", fmt.Sprintf(`{"state":%q}`, started.State))
	if !strings.Contains(cancel.Body.String(), `"cancelled":true`) {
		t.Fatalf("cancel: %s", cancel.Body)
	}
	after := oauthAdminCall(s, "/workbuddy/login/poll", fmt.Sprintf(`{"state":%q}`, started.State))
	if !strings.Contains(after.Body.String(), `"status":"unknown"`) {
		t.Fatalf("poll after cancel: %s", after.Body)
	}
}
