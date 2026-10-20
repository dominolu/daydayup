package workbuddy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// jwt builds a fake access token whose payload carries the given claims so
// tests can exercise uid/exp extraction without a real upstream login.
func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return head + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestStartAuth(t *testing.T) {
	const state = "state-1"
	var started bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started = true
		if r.Method != http.MethodPost || r.URL.Path != authStatePath {
			t.Fatalf("want POST %s, got %s %s", authStatePath, r.Method, r.URL.Path)
		}
		if q := r.URL.Query().Get("platform"); q != "CLI" {
			t.Fatalf("platform query %q", q)
		}
		if r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Origin") == "" {
			t.Fatalf("browser headers missing: %v", r.Header)
		}
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"state": state, "authUrl": "https://www.workbuddy.ai/login?state=" + state}})
	}))
	defer up.Close()
	restore := OverrideUpstream("intl", up.URL)
	defer restore()

	start, err := (WorkBuddy{}).StartAuth(context.Background(), up.Client(), "intl", "CLI")
	if err != nil {
		t.Fatal(err)
	}
	if !started || start.State != state || start.Realm != "intl" || start.Platform != "CLI" {
		t.Fatalf("start: %+v started=%v", start, started)
	}
}

func TestStartAuthUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 40001, "msg": "rate limited"})
	}))
	defer up.Close()
	restore := OverrideUpstream("intl", up.URL)
	defer restore()
	if _, err := (WorkBuddy{}).StartAuth(context.Background(), up.Client(), "intl", "CLI"); err == nil || !strings.Contains(err.Error(), "40001") {
		t.Fatalf("want code error, got %v", err)
	}
}

func TestPollAuthPending(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": loginPendingCode, "msg": "login ing..."})
	}))
	defer up.Close()
	restore := OverrideUpstream("intl", up.URL)
	defer restore()
	res, err := (WorkBuddy{}).PollAuth(context.Background(), up.Client(), "intl", "state-x")
	if err != nil || res.Status != "pending" {
		t.Fatalf("want pending, got %+v err=%v", res, err)
	}
}

func TestPollAuthSuccess(t *testing.T) {
	const nick = "主号测试"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == authStatePath {
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"state": "s", "authUrl": "https://x/login?state=s"}})
			return
		}
		if r.URL.Path == authTokenPath {
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
				"accessToken":  jwt(t, map[string]any{"sub": "uid-123", "exp": float64(time.Now().Add(2 * time.Hour).Unix())}),
				"refreshToken": "refresh-xyz",
				"expiresAt":    float64(time.Now().Add(3 * time.Hour).UnixMilli() / 1000),
			}})
			return
		}
		if r.URL.Path == loginAccountPath {
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
				t.Fatalf("profile authorization header %q", got)
			}
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"nickname": nick}})
			return
		}
		http.NotFound(w, r)
	}))
	defer up.Close()
	restore := OverrideUpstream("intl", up.URL)
	defer restore()

	res, err := (WorkBuddy{}).PollAuth(context.Background(), up.Client(), "intl", "s")
	if err != nil || res.Status != "ok" {
		t.Fatalf("want ok, got %+v err=%v", res, err)
	}
	a := res.Account
	if a.UID != "uid-123" || a.AccessToken == "" || a.RefreshToken != "refresh-xyz" {
		t.Fatalf("account: %+v", a)
	}
	if a.Nickname != nick {
		t.Fatalf("nickname %q want %q", a.Nickname, nick)
	}
	if time.Until(a.ExpiresAt) < time.Hour {
		t.Fatalf("expiresAt %v too soon", a.ExpiresAt)
	}
}

func TestJWTClaimsHelpers(t *testing.T) {
	tok := jwt(t, map[string]any{"sub": "u-9", "exp": float64(1900000000)})
	if uid := jwtUID(tok); uid != "u-9" {
		t.Fatalf("jwtUID %q", uid)
	}
	if exp := jwtExp(tok); exp.Unix() != 1900000000 {
		t.Fatalf("jwtExp %v", exp)
	}
	if uid := jwtUID("not-a-jwt"); uid != "" {
		t.Fatalf("malformed uid %q", uid)
	}
	if !jwtExp("bad").IsZero() {
		t.Fatal("malformed exp must be zero")
	}
}

func TestNormalizeEpoch(t *testing.T) {
	sec := time.Unix(1700000000, 0)
	ms := time.Unix(1700000000, 0)
	if got := normalizeEpoch(float64(1700000000)); !got.Equal(sec) {
		t.Fatalf("seconds: %v", got)
	}
	if got := normalizeEpoch(float64(1700000000000)); !got.Equal(ms) {
		t.Fatalf("milliseconds: %v", got)
	}
	if got := normalizeEpoch("1700000000"); !got.Equal(sec) {
		t.Fatalf("string: %v", got)
	}
	if !normalizeEpoch(nil).IsZero() {
		t.Fatal("nil must be zero")
	}
}