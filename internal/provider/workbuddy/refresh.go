package workbuddy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"daydayup/internal/config"
)

const refreshPath = "/v2/plugin/auth/token/refresh"

// jwtClaims decodes the unverified payload of a JWT access token. The gateway
// only reads structural claims (exp, sub) and never verifies signatures; a
// malformed token yields nil so callers treat it as unknown.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

// jwtExp parses the exp claim from a JWT access token without verifying the
// signature (the gateway only needs the expiry window for proactive refresh).
// A malformed token yields zero so callers treat it as unknown and refresh on
// the conservative side.
func jwtExp(token string) time.Time {
	claims := jwtClaims(token)
	if claims == nil {
		return time.Time{}
	}
	exp, ok := numberClaim(claims["exp"])
	if !ok || exp == 0 {
		return time.Time{}
	}
	return time.Unix(int64(exp), 0)
}

// jwtUID returns the sub claim, the account uid the upstream login flow
// issues. Empty when the token is malformed or carries no sub.
func jwtUID(token string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", claims["sub"]))
}

// numberClaim reads a JSON number claim that may arrive as float64 or string.
func numberClaim(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// AccessTokenExpiry returns the parsed JWT expiry of the account's access
// token, or the zero time when the token carries no parseable exp.
func AccessTokenExpiry(a config.Account) time.Time {
	return jwtExp(a.APIKey)
}

// Refresh rotates the OAuth access token in place. The upstream rotates the
// refresh token too, so callers must swap both atomically (per-account lock
// on the gateway side) and persist the result.
func (w WorkBuddy) Refresh(ctx context.Context, client *http.Client, a config.Account) (config.Account, error) {
	if a.RefreshToken == "" {
		return a, errors.New("workbuddy: no refresh token; sign in again")
	}
	rc := realmOf(a)
	headers := map[string]string{
		"Content-Type":        "application/json",
		"Accept":              "application/json, text/plain, */*",
		"X-Requested-With":    "XMLHttpRequest",
		"User-Agent":          rc.billingUA,
		"Origin":              rc.origin,
		"Referer":             rc.origin + "/",
		"X-Refresh-Token":     a.RefreshToken,
		"X-User-Id":           uidOf(a),
		"X-Domain":            rc.domainFor(a),
		"X-CodeBuddy-Request": "1",
		"Accept-Language":     rc.acceptLang,
	}
	if isCN(a) {
		headers["X-Auth-Refresh-Source"] = "workbuddy"
	} else {
		headers["X-Auth-Refresh-Source"] = "plugin"
	}
	if a.EnterpriseID != "" {
		headers["X-Enterprise-Id"] = a.EnterpriseID
	}
	chat := rc.chatUpstream
	if a.BaseURL != "" && a.BaseURL != rc.chatUpstream {
		chat = a.BaseURL
	}
	b, status, err := doJSON(ctx, client, http.MethodPost, chat+refreshPath, headers, []byte("{}"), 1<<20)
	if err != nil {
		return a, err
	}
	if status != 200 {
		return a, &HTTPStatusError{Status: status}
	}
	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return a, errors.New("workbuddy: invalid refresh response")
	}
	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	// The response nests data inside data for some realms.
	if inner := rawDict(payload.Data); inner != nil {
		_ = json.Unmarshal(payload.Data, &data)
		_ = json.Unmarshal(inner["data"], &data)
	} else {
		_ = json.Unmarshal(payload.Data, &data)
	}
	if data.AccessToken == "" {
		return a, errors.New("workbuddy: refresh returned no access token")
	}
	if data.RefreshToken == "" {
		data.RefreshToken = a.RefreshToken
	}
	out := a
	out.APIKey = data.AccessToken
	out.RefreshToken = data.RefreshToken
	return out, nil
}

// HTTPStatusError is a refresh transport failure with a status code.
type HTTPStatusError struct {
	Status int
}

func (e *HTTPStatusError) Error() string {
	return "workbuddy: refresh HTTP " + itoa(e.Status)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
