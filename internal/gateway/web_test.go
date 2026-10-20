package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daydayup/internal/config"
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
