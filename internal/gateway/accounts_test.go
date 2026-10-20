package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daydayup/internal/pool"
)

func adminCall(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestAccountCRUDPersistsAndNeverReturnsCredentials(t *testing.T) {
	s, up := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}), 1)
	s.Config.DBFile = filepath.Join(t.TempDir(), "gateway.db")
	s.Config.StateFile = filepath.Join(t.TempDir(), "state.json")
	s = New(s.Config)
	if err := s.LoadAccounts(); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"id":"ui","name":"<script>alert(1)</script>","api_key":"private-ui-key","base_url":%q,"max_in_flight":3}`, up.URL)
	w := adminCall(s, "POST", "/admin/v1/accounts", body)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "private-ui-key") {
		t.Fatal("credential returned")
	}
	if adminCall(s, "POST", "/admin/v1/accounts", body).Code != 409 {
		t.Fatal("duplicate account accepted")
	}
	w = adminCall(s, "POST", "/admin/v1/accounts/ui/check", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"credential_status":"valid"`) {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	}
	w = adminCall(s, "PUT", "/admin/v1/accounts/ui", `{"name":"renamed","api_key":"","max_in_flight":2}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	a, _ := s.Pool.Account("ui")
	if a.APIKey != "private-ui-key" || a.Name != "renamed" || a.MaxInFlight != 2 {
		t.Fatalf("edit lost fields: %+v", a)
	}
	loaded := New(s.Config)
	if err := loaded.LoadAccounts(); err != nil {
		t.Fatal(err)
	}
	a, err := loaded.Pool.Account("ui")
	if err != nil || a.APIKey != "private-ui-key" {
		t.Fatal("account did not survive restart")
	}
	if adminCall(s, "DELETE", "/admin/v1/accounts/a0", "").Code != 200 {
		t.Fatal("configured account removal failed")
	}
	loaded = New(s.Config)
	if err := loaded.LoadAccounts(); err != nil {
		t.Fatal(err)
	}
	if _, err := loaded.Pool.Account("a0"); err == nil {
		t.Fatal("removed config account resurrected")
	}
	if adminCall(s, "DELETE", "/admin/v1/accounts/ui", "").Code != 200 {
		t.Fatal("delete failed")
	}
	if len(s.Pool.Snapshot()) != 0 {
		t.Fatal("last account cannot be removed")
	}
}

func TestBusyAccountCannotBeReplacedOrDeleted(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), 1)
	a, err := s.Pool.Acquire("sensenova", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if adminCall(s, "PUT", "/admin/v1/accounts/a0", `{"api_key":"changed-key"}`).Code != 409 {
		t.Fatal("active credential replaced")
	}
	if adminCall(s, "DELETE", "/admin/v1/accounts/a0", "").Code != 409 {
		t.Fatal("active account removed")
	}
	s.Pool.Finish(a.ID, 200, pool.Usage{}, false, 0, "")
	if adminCall(s, "PUT", "/admin/v1/accounts/a0", `{"api_key":"changed-key"}`).Code != 200 {
		t.Fatal("idle key rotation failed")
	}
	state := s.Pool.Snapshot()[0]
	if state.CredentialStatus != "unknown" || len(state.Models) != 0 {
		t.Fatal("new credential inherited old health")
	}
}

func TestEmbeddedAdminAndMetadata(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), 1)
	for _, path := range []string{"/admin/", "/admin/assets/app.js", "/admin/assets/app.css"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("admin asset %s: %d", path, w.Code)
		}
	}
	w := adminCall(s, "GET", "/admin/v1/info", "")
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result["account_storage"] != "AES-256-GCM" || strings.Contains(w.Body.String(), s.Config.AdminKey) {
		t.Fatal("invalid metadata")
	}
	w = adminCall(s, "POST", "/admin/v1/accounts", `{"id":"missing-key"}`)
	if w.Code != 400 {
		t.Fatal("empty credential accepted")
	}
}

func TestDynamicAccountsParticipateInRetry(t *testing.T) {
	s, up := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer upstream-secret-0" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":2}}`)
	}), 1)
	body := fmt.Sprintf(`{"id":"new-live","api_key":"new-managed-key","base_url":%q}`, up.URL)
	if w := adminCall(s, "POST", "/admin/v1/accounts", body); w.Code != 201 {
		t.Fatal(w.Body)
	}
	if w := call(s, "POST", "/v1/chat/completions", `{"model":"m"}`); w.Code != 200 {
		t.Fatalf("runtime account not eligible for retry: %d %s", w.Code, w.Body)
	}
	state := s.Pool.Snapshot()
	if state[0].Stats.LastStatus != 401 || state[1].Stats.Total != 2 {
		t.Fatal("request did not fail over to added account")
	}
}

func TestFailedPersistenceDoesNotActivateAccount(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), 1)
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Config.DBFile = filepath.Join(parent, "gateway.db")
	s = New(s.Config)
	// The unified database cannot be created here: the gateway must fail
	// closed at startup (main checks StoreError) instead of ever serving or
	// persisting an account against a broken store.
	if s.StoreError() == nil {
		t.Fatal("unopenable database accepted")
	}
	if len(s.Pool.Accounts()) != 1 {
		t.Fatal("pool mutated on failed startup")
	}
}
