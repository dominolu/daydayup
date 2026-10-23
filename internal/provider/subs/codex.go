package subs

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The ChatGPT subscription behind Codex CLI. Codex keeps its sign-in in
// auth.json (CODEX_HOME, ~/.codex by default) and refreshes it with OpenAI's
// OAuth endpoint; a refresh rotates the token pair, so the rotated values are
// written back for Codex to find.
const (
	codexClientID   = "app_EMoamEEZ73f0CkXaXp7hrann" // Codex CLI's own OAuth client
	codexTokenURL   = "https://auth.openai.com/oauth/token"
	codexBase       = "https://chatgpt.com/backend-api/codex"
	codexOriginator = "codex_cli_rs"
	codexVersion    = "0.159.0"
)

// CodexBase is where a ChatGPT account's Codex requests go.
func CodexBase() string { return codexBase }

// codexAuth is the part of Codex CLI's auth.json this package reads.
type codexAuth struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

// CodexAuthPath is Codex CLI's auth.json for this user.
func CodexAuthPath() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "auth.json")
	}
	home, err := homeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

type codexCred struct {
	mu   sync.Mutex
	path string
	tok  string
	exp  time.Time
}

func openCodex(path string) (Credential, error) {
	if path == "" {
		path = CodexAuthPath()
	}
	if path == "" {
		return nil, ErrSignedOut
	}
	return cached("codex\x00"+path, func() (Credential, error) {
		c := &codexCred{path: path}
		if _, err := c.read(); err != nil {
			return nil, err
		}
		return c, nil
	})
}

func (c *codexCred) read() (codexAuth, error) {
	var raw map[string]any
	if err := readJSONFile(c.path, &raw); err != nil {
		if os.IsNotExist(err) {
			return codexAuth{}, ErrSignedOut
		}
		return codexAuth{}, err
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return codexAuth{}, err
	}
	var a codexAuth
	if err := json.Unmarshal(b, &a); err != nil {
		return codexAuth{}, err
	}
	return a, nil
}

// Identity describes the signed-in ChatGPT account.
func (c *codexCred) Identity() Identity {
	a, err := c.read()
	if err != nil {
		return Identity{Kind: Codex, Source: c.path}
	}
	return codexIdentity(c.path, a)
}

func codexIdentity(path string, a codexAuth) Identity {
	claims := jwtClaims(a.Tokens.IDToken)
	accountID := a.Tokens.AccountID
	if accountID == "" {
		accountID = claimString(claims, "https://api.openai.com/auth", "chatgpt_account_id")
	}
	user := claimString(claims, "email")
	if user == "" {
		user = claimString(claims, "name")
	}
	return Identity{
		Kind:      Codex,
		User:      user,
		Plan:      claimString(claims, "https://api.openai.com/auth", "chatgpt_plan_type"),
		AccountID: accountID,
		Source:    path,
		ExpiresAt: expiryOf(a.Tokens.AccessToken),
	}
}

// Token returns a usable access token for the Codex sign-in,
// refreshing it when it is about to lapse.
func (c *codexCred) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.read()
	if err != nil {
		return "", err
	}
	if a.AuthMode != "" && a.AuthMode != "chatgpt" {
		return "", errNotSubscription
	}
	if a.Tokens.AccessToken == "" {
		return "", ErrSignedOut
	}
	if exp := expiryOf(a.Tokens.AccessToken); fresh(exp) {
		c.tok, c.exp = a.Tokens.AccessToken, exp
		return c.tok, nil
	}
	return c.refresh(ctx, a)
}

var errNotSubscription = errSignedInWithKey

func (c *codexCred) refresh(ctx context.Context, a codexAuth) (string, error) {
	if a.Tokens.RefreshToken == "" {
		return "", ErrSignedOut
	}
	// A vendored read would miss a rotation Codex made since; the file is
	// read again by the caller, which is fresh enough for the refresh below.
	rctx, cancel := withoutCancel(ctx)
	defer cancel()
	body, err := json.Marshal(map[string]string{
		"client_id":     codexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": a.Tokens.RefreshToken,
		"scope":         "openid profile email",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, codexTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var fresh struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, res.Body, 1<<20))
	_ = dec.Decode(&fresh)
	if res.StatusCode != http.StatusOK || fresh.AccessToken == "" {
		// 400/401 is a refusal (the sign-in is gone); a 5xx may pass.
		if res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusUnauthorized {
			return "", ErrSignedOut
		}
		return "", errRefreshFailed(Codex, res.StatusCode)
	}
	c.store(fresh.AccessToken, fresh.IDToken, fresh.RefreshToken)
	return fresh.AccessToken, nil
}

// store writes a rotated token pair back to Codex's own auth.json, keeping
// every other field Codex wrote untouched.
func (c *codexCred) store(access, id, refresh string) {
	var raw map[string]any
	if err := readJSONFile(c.path, &raw); err != nil {
		raw = map[string]any{}
	}
	tokens, _ := raw["tokens"].(map[string]any)
	if tokens == nil {
		tokens = map[string]any{}
	}
	tokens["access_token"] = access
	if id != "" {
		tokens["id_token"] = id
	}
	if refresh != "" {
		tokens["refresh_token"] = refresh
	}
	raw["tokens"] = tokens
	raw["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	if out, err := json.MarshalIndent(raw, "", "  "); err == nil {
		// A write that fails still serves this request with the fresh token.
		_ = writeFileAtomic(c.path, out)
	}
}

// Sign authenticates a request the way Codex CLI does: the ChatGPT backend
// serves Codex's clients and gates its newest models on the client identity.
func (c *codexCred) Sign(ctx context.Context, req *http.Request) error {
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	id := c.Identity()
	req.Header.Set("Authorization", "Bearer "+tok)
	if id.AccountID != "" {
		req.Header.Set("chatgpt-account-id", id.AccountID)
	}
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", codexOriginator)
	req.Header.Set("version", codexVersion)
	req.Header.Set("User-Agent", "codex_cli_rs/"+codexVersion+" (macOS; arm64) Codex CLI")
	return nil
}

// Invalidate drops the cached token after the upstream rejected it.
func (c *codexCred) Invalidate() {
	c.mu.Lock()
	c.tok, c.exp = "", time.Time{}
	c.mu.Unlock()
}

// discoverCodex lists the Codex sign-in on this machine.
func discoverCodex() ([]Identity, error) {
	path := CodexAuthPath()
	if path == "" {
		return nil, nil
	}
	var raw map[string]any
	if err := readJSONFile(path, &raw); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	b, _ := json.Marshal(raw)
	var a codexAuth
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	if a.Tokens.AccessToken == "" || (a.AuthMode != "" && a.AuthMode != "chatgpt") {
		return nil, nil
	}
	return []Identity{codexIdentity(path, a)}, nil
}
