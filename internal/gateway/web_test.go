package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"safuno/internal/config"
)

func TestClientKeyRequiresAdminAndIsNotIncludedInOtherResponses(t *testing.T) {
	s := New(config.Config{
		GatewayKey: "gateway-client-secret-1234",
		AdminKey:   "gateway-admin-secret-5678",
		Accounts: []config.Account{{
			ID: "account-1", Provider: "sensenova", APIKey: "upstream-secret-9012",
		}},
	})
	for _, key := range []string{"", "wrong-key", s.Config.GatewayKey} {
		r := httptest.NewRequest(http.MethodGet, "/admin/v1/client-key", nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), s.Config.GatewayKey) {
			t.Fatalf("client key disclosed without admin authorization: HTTP %d", w.Code)
		}
	}
	w := adminCall(s, http.MethodGet, "/admin/v1/client-key", "")
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || result["api_key"] != s.Config.GatewayKey || len(result) != 1 {
		t.Fatal("admin could not read the current client key")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("client key response must not be cached")
	}
	if strings.Contains(w.Body.String(), s.Config.AdminKey) || strings.Contains(w.Body.String(), s.Config.Accounts[0].APIKey) {
		t.Fatal("client key response included another credential")
	}
	for _, path := range []string{"/admin/", "/admin/assets/app.js", "/admin/v1/info", "/admin/v1/accounts"} {
		w = adminCall(s, http.MethodGet, path, "")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), s.Config.GatewayKey) {
			t.Fatalf("client key should only be returned by its explicit endpoint: %s", path)
		}
	}
	w = adminCall(s, http.MethodPost, "/admin/v1/client-key", "")
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), s.Config.GatewayKey) {
		t.Fatal("client key endpoint must be read only")
	}
}

// TestAdminConsoleNeedsNoLogin pins the no-login contract: loading /admin/
// issues the same-origin session automatically, the page contains no login
// form, and the admin API stays closed for requests without either the
// session or the explicit Bearer key.
func TestAdminConsoleNeedsNoLogin(t *testing.T) {
	s := New(config.Config{
		GatewayKey: "gateway-client-secret-1234",
		AdminKey:   "gateway-admin-secret-5678",
	})

	page := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, page)
	if w.Code != http.StatusOK {
		t.Fatalf("console page: HTTP %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "login-overlay") || strings.Contains(w.Body.String(), "admin-key") {
		t.Fatal("console page still renders the login form")
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == adminSessionCookie {
			session = c
		}
	}
	if session == nil || session.Value == "" || !session.HttpOnly || session.Path != "/admin" {
		t.Fatalf("console page did not issue the expected session cookie: %+v", session)
	}
	if strings.Contains(w.Body.String(), session.Value) {
		t.Fatal("session value leaked into the page body")
	}

	// No credential and no session: still unauthorized.
	anonymous := httptest.NewRequest(http.MethodGet, "/admin/v1/info", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, anonymous)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous admin call: HTTP %d, want 401", w.Code)
	}

	// The automatic same-origin session is enough; no key prompt.
	console := httptest.NewRequest(http.MethodGet, "/admin/v1/info", nil)
	console.AddCookie(session)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, console)
	if w.Code != http.StatusOK {
		t.Fatalf("console session admin call: HTTP %d, want 200", w.Code)
	}
}
