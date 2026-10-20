package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Browser OAuth login: the gateway asks the upstream plugin auth endpoint for
// a one-time login state and an authorization URL, opens that URL in the
// user's browser, and polls until the browser login completes. The same
// endpoints and header family as the official desktop client and the
// workbuddy2api-hub panel; the gateway never touches the browser itself.
const (
	authStatePath    = "/v2/plugin/auth/state"
	authTokenPath    = "/v2/plugin/auth/token"
	loginAccountPath = "/v2/plugin/login/account"

	// loginPendingCode is the upstream code while the browser login is still
	// in progress (poll returns it repeatedly until the user finishes).
	loginPendingCode = 11217

	// LoginTTL is how long a login state stays usable before the gateway
	// drops it, matching the hub's 600s window.
	LoginTTL = 10 * time.Minute
)

// AuthStart is the response of starting a browser OAuth login: the one-time
// state the gateway must remember, and the authorization URL the browser
// should open.
type AuthStart struct {
	State    string `json:"state"`
	AuthURL  string `json:"auth_url"`
	Realm    string `json:"realm"`
	Platform string `json:"platform"`
}

// AuthAccount is the fully extracted credential set returned once the browser
// login completes: uid and tokens with the upstream's expiry, plus the
// profile nickname when the profile endpoint answers.
type AuthAccount struct {
	UID          string    `json:"uid"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Nickname     string    `json:"nickname"`
	Domain       string    `json:"domain"`
	Realm        string    `json:"realm"`
}

// AuthResult is the poll outcome. Pending means the browser login has not
// finished; OK carries the extracted credentials; errors are returned as Go
// errors so the gateway can surface the upstream message.
type AuthResult struct {
	Status  string       `json:"status"` // "pending" | "ok"
	Message string       `json:"message,omitempty"`
	Account *AuthAccount `json:"account,omitempty"`
}

// authHeaders is the browser-ish header family the plugin auth endpoints
// expect (same as the hub's start_login / poll_login).
func authHeaders(rc realmConfig) map[string]string {
	return map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "application/json, text/plain, */*",
		"X-Requested-With": "XMLHttpRequest",
		"User-Agent":       rc.billingUA,
		"Origin":           rc.origin,
		"Referer":          rc.origin + "/",
	}
}

// StartAuth requests a new browser OAuth login for the given realm. platform
// is only echoed by the upstream login page (the account the login creates is
// configured separately by the gateway caller).
func (WorkBuddy) StartAuth(ctx context.Context, client *http.Client, realm, platform string) (*AuthStart, error) {
	rc := realmConfigFor(realm)
	u := rc.chatUpstream + authStatePath + "?platform=" + url.QueryEscape(platform)
	b, status, err := doJSON(ctx, client, http.MethodPost, u, authHeaders(rc), []byte("{}"), 1<<20)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("workbuddy: auth state HTTP %d", status)
	}
	var payload struct {
		Code int `json:"code"`
		Msg  string
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return nil, errors.New("workbuddy: invalid auth state response")
	}
	if payload.Code != 0 {
		return nil, fmt.Errorf("workbuddy: auth state code=%d msg=%s", payload.Code, payload.Msg)
	}
	if payload.Data.State == "" || payload.Data.AuthURL == "" {
		return nil, errors.New("workbuddy: auth state returned no state/authUrl")
	}
	return &AuthStart{
		State:    payload.Data.State,
		AuthURL:  payload.Data.AuthURL,
		Realm:    realm,
		Platform: platform,
	}, nil
}

// PollAuth checks whether the browser login for state has completed. It
// returns AuthResult{Status:"pending"} while the user is still logging in,
// AuthResult{Status:"ok"} with the extracted credentials once done, and an
// error when the upstream rejects the state or the response is malformed.
func (WorkBuddy) PollAuth(ctx context.Context, client *http.Client, realm, state string) (*AuthResult, error) {
	rc := realmConfigFor(realm)
	u := rc.chatUpstream + authTokenPath + "?state=" + url.QueryEscape(state)
	b, status, err := doJSON(ctx, client, http.MethodGet, u, authHeaders(rc), nil, 1<<20)
	if err != nil {
		return &AuthResult{Status: "pending", Message: "poll error: " + err.Error()}, nil
	}
	if status != 200 {
		return &AuthResult{Status: "error", Message: fmt.Sprintf("poll HTTP %d", status)}, nil
	}
	var payload struct {
		Code int `json:"code"`
		Msg  string
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
			Domain       string `json:"domain"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return nil, errors.New("workbuddy: invalid auth token response")
	}
	if payload.Code == loginPendingCode {
		return &AuthResult{Status: "pending", Message: payload.Msg}, nil
	}
	if payload.Code != 0 {
		return &AuthResult{Status: "error", Message: fmt.Sprintf("code=%d msg=%s", payload.Code, payload.Msg)}, nil
	}
	token := strings.TrimSpace(payload.Data.AccessToken)
	if token == "" {
		return &AuthResult{Status: "pending", Message: "waiting for token"}, nil
	}
	uid := jwtUID(token)
	if uid == "" {
		return nil, errors.New("workbuddy: login token carries no sub claim")
	}
	exp := normalizeEpoch(payload.Data.ExpiresAt)
	if exp.IsZero() {
		exp = jwtExp(token)
	}
	acc := &AuthAccount{
		UID:          uid,
		AccessToken:  token,
		RefreshToken: strings.TrimSpace(payload.Data.RefreshToken),
		ExpiresAt:    exp,
		Domain:       strings.TrimSpace(payload.Data.Domain),
		Realm:        realm,
	}
	// The profile endpoint (same login session) returns the nickname; a
	// failure here is cosmetic, the tokens are already valid.
	if nick, ok := fetchLoginNickname(ctx, client, rc, state, token); ok {
		acc.Nickname = nick
	}
	return &AuthResult{Status: "ok", Message: "ok", Account: acc}, nil
}

// fetchLoginNickname resolves the account profile behind an in-progress login
// state (Authorization: Bearer <new access token>).
func fetchLoginNickname(ctx context.Context, client *http.Client, rc realmConfig, state, token string) (string, bool) {
	headers := authHeaders(rc)
	headers["Authorization"] = "Bearer " + token
	u := rc.chatUpstream + loginAccountPath + "?state=" + url.QueryEscape(state)
	b, status, err := doJSON(ctx, client, http.MethodGet, u, headers, nil, 1<<20)
	if err != nil || status != 200 {
		return "", false
	}
	var payload struct {
		Data struct {
			Nickname string `json:"nickname"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return "", false
	}
	return strings.TrimSpace(payload.Data.Nickname), payload.Data.Nickname != ""
}

// normalizeEpoch converts the upstream expiresAt value (epoch seconds or
// milliseconds, number or string) into a time; zero when absent/malformed.
func normalizeEpoch(v any) time.Time {
	switch n := v.(type) {
	case float64:
		return epochTime(n)
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return epochTime(f)
		}
	}
	return time.Time{}
}

func epochTime(msOrSec float64) time.Time {
	if msOrSec == 0 {
		return time.Time{}
	}
	if msOrSec > 1e11 {
		msOrSec /= 1000
	}
	return time.Unix(int64(msOrSec), 0)
}

// realmConfigFor returns the realm config, defaulting to intl exactly like
// the hub's get_realm_config.
func realmConfigFor(realm string) realmConfig {
	if rc, ok := realms[realm]; ok {
		return rc
	}
	return realms["intl"]
}

// OverrideUpstream temporarily replaces the chat and billing upstream origins
// a realm uses (browser OAuth, check-in tasks and quota calls share these
// hosts). It exists for tests and for self-hosted mirrors that front the
// official endpoints; the returned function restores the realm default. Do
// not call it while live logins or tasks are running.
func OverrideUpstream(realm, base string) func() {
	rc := realmConfigFor(realm)
	prevChat, prevBilling := rc.chatUpstream, rc.billingUpstream
	base = strings.TrimRight(base, "/")
	rc.chatUpstream = base
	rc.billingUpstream = base
	realms[realm] = rc
	return func() {
		restore := realmConfigFor(realm)
		restore.chatUpstream = prevChat
		restore.billingUpstream = prevBilling
		realms[realm] = restore
	}
}
