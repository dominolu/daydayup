// Package subs serves the subscription sign-ins the vendor CLIs keep on this
// machine — Codex's ChatGPT login, Claude Code's OAuth login and Copilot's
// GitHub login — as gateway credentials.
//
// A sign-in is never copied into the account store: the gateway reads the
// CLI's own file (or keychain item), refreshes the token the way that CLI
// does, and writes a rotated token back where the CLI will find it. The CLI
// and the gateway therefore share one sign-in instead of competing for it;
// two holders of the same rotating refresh token would invalidate each other.
package subs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Kind names one subscription a machine can be signed in to.
type Kind string

const (
	Codex   Kind = "codex"
	Claude  Kind = "claude"
	Copilot Kind = "copilot"
)

// Kinds lists the subscriptions the gateway serves, in console order.
func Kinds() []Kind { return []Kind{Codex, Claude, Copilot} }

// ErrSignedOut is a sign-in that is gone: the vendor refused the refresh, so
// the CLI has to sign in again. Waiting does not mend it.
var ErrSignedOut = errors.New("subscription sign-in expired; sign in again with the vendor CLI")

// errSignedInWithKey says the CLI holds an API key, not a subscription
// sign-in: the gateway serves subscriptions, and an API key is added as a
// normal account instead.
var errSignedInWithKey = errors.New("the CLI is signed in with an API key, not a subscription")

// errRefreshFailed is a refresh that did not go through for a reason worth
// trying again (a hiccup, a proxy in the way).
func errRefreshFailed(kind Kind, status int) error {
	return fmt.Errorf("subs: %s token refresh: failed (HTTP %d)", kind, status)
}

// Identity is who one local sign-in belongs to. It carries no secret: the
// admin console lists identities, never tokens.
type Identity struct {
	Kind      Kind      `json:"kind"`
	User      string    `json:"user"`
	Plan      string    `json:"plan,omitempty"`
	AccountID string    `json:"account_id,omitempty"`
	Source    string    `json:"source"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// Credential is one local subscription sign-in. Implementations cache the
// access token, refresh it before it expires, and share the lock that guards
// the underlying file with every other Credential of the same file.
type Credential interface {
	// Identity describes the account, without secrets.
	Identity() Identity
	// Token returns a usable access token, refreshing it when it is about
	// to expire.
	Token(ctx context.Context) (string, error)
	// Sign authenticates an upstream request the way the vendor CLI does
	// (bearer token plus the vendor's own identity headers).
	Sign(ctx context.Context, req *http.Request) error
	// Invalidate drops the cached token after the upstream rejected it, so
	// the next call refreshes instead of replaying it.
	Invalidate()
}

// Open returns the credential of one subscription, reading the CLI's own
// sign-in at path ("" means its default location). Repeated calls for the
// same source return the same instance, so a refresh is shared.
func Open(kind Kind, path string) (Credential, error) {
	var (
		c   Credential
		err error
	)
	switch kind {
	case Codex:
		c, err = openCodex(path)
	case Claude:
		c, err = openClaude(path)
	case Copilot:
		c, err = openCopilot(path)
	default:
		return nil, fmt.Errorf("subs: %q is not a subscription this gateway serves", kind)
	}
	return c, err
}

// Discover lists the sign-ins of one subscription found on this machine.
func Discover(kind Kind) ([]Identity, error) {
	switch kind {
	case Codex:
		return discoverCodex()
	case Claude:
		return discoverClaude()
	case Copilot:
		return discoverCopilot()
	}
	return nil, fmt.Errorf("subs: %q is not a subscription this gateway serves", kind)
}

// ---- shared machinery ------------------------------------------------------

var (
	openMu sync.Mutex
	opened = map[string]Credential{}
)

// cached returns the credential already opened for key, or opens one.
func cached(key string, open func() (Credential, error)) (Credential, error) {
	openMu.Lock()
	defer openMu.Unlock()
	if c, ok := opened[key]; ok {
		return c, nil
	}
	c, err := open()
	if err != nil {
		return nil, err
	}
	opened[key] = c
	return c, nil
}

// reset forgets every opened credential; tests use it between cases so a
// rewritten file is read again.
func reset() {
	openMu.Lock()
	opened = map[string]Credential{}
	openMu.Unlock()
}

// client is what token refreshes go through: they must not hang a request.
var client = &http.Client{Timeout: 60 * time.Second}

// jwtClaims decodes a token's payload without verifying it. These are the
// user's own tokens and only the expiry and subject matter here.
func jwtClaims(token string) map[string]any {
	parts := splitToken(token)
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}

func splitToken(token string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			parts = append(parts, token[start:i])
			start = i + 1
		}
	}
	return append(parts, token[start:])
}

// claimString reads a nested string claim.
func claimString(claims map[string]any, keys ...string) string {
	var value any = claims
	for _, key := range keys {
		m, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = m[key]
	}
	s, _ := value.(string)
	return s
}

// claimTime reads a nested Unix-seconds claim.
func claimTime(claims map[string]any, keys ...string) time.Time {
	var value any = claims
	for _, key := range keys {
		m, ok := value.(map[string]any)
		if !ok {
			return time.Time{}
		}
		value = m[key]
	}
	switch v := value.(type) {
	case float64:
		if v > 0 {
			return time.Unix(int64(v), 0)
		}
	}
	return time.Time{}
}

// expiryOf reads a JWT's exp, the zero time when it has none.
func expiryOf(token string) time.Time {
	if claims := jwtClaims(token); claims != nil {
		return claimTime(claims, "exp")
	}
	return time.Time{}
}

// writeFileAtomic replaces path with data, keeping the file's mode. A crash
// mid-write leaves the CLI's old sign-in whole instead of truncated.
func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// homeDir is the user's home, or an error that names what is missing.
func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("subs: cannot locate the home directory of this user")
	}
	return home, nil
}

// readJSONFile decodes path into v.
func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("subs: %s is not valid JSON: %w", path, err)
	}
	return nil
}

// refreshWindow is how long before expiry a token is refreshed: a request
// already on its way must not reach the vendor with a token about to lapse.
const refreshWindow = 5 * time.Minute

// fresh reports whether a token may still be used.
func fresh(exp time.Time) bool {
	return !exp.IsZero() && time.Until(exp) > refreshWindow
}

// withoutCancel detaches a refresh from the caller giving up: the vendor
// rotates the refresh token as it answers, so an abandoned refresh would
// leave the spent token in memory and the new one lost.
func withoutCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}
