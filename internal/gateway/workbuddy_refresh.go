package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"safuno/internal/config"
	"safuno/internal/provider"
	"safuno/internal/provider/workbuddy"
)

// tokenLocks serializes token refresh per account: the upstream rotates the
// refresh token on every refresh, so two concurrent refreshes would make the
// second one present an already-consumed token.
var tokenLocks sync.Map

// refreshAccountTokens rotates WorkBuddy credentials in place and persists
// the swapped tokens to the encrypted account store. Changed is true when a
// refresh happened (or was already done by a concurrent goroutine); the
// caller retries the request with the returned account.
func (s *Server) refreshAccountTokens(ctx context.Context, a config.Account) (config.Account, bool, error) {
	p, ok := s.providers.Get(a.Provider)
	if !ok {
		return a, false, nil
	}
	refreshable, ok := p.(provider.Refreshable)
	if !ok || a.RefreshToken == "" {
		return a, false, nil
	}
	mu, _ := tokenLocks.LoadOrStore(a.ID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	// Re-read under the lock: a concurrent refresh may already have rotated.
	if cur, err := s.Pool.Account(a.ID); err == nil && cur.RefreshToken != "" && (cur.APIKey != a.APIKey || cur.RefreshToken != a.RefreshToken) {
		return cur, true, nil
	}
	refreshed, err := refreshable.Refresh(ctx, s.client, a)
	if err != nil {
		return a, false, err
	}
	if err := s.Pool.UpdateCredentials(refreshed, s.accountStore.SaveAccount); err != nil {
		return a, false, err
	}
	slog.Info("workbuddy token refreshed", "account", a.ID)
	return refreshed, true, nil
}

// refreshWorkBuddyLoop scans workbuddy accounts and rotates access tokens
// that are expired or within the proactive window. Runs on the background
// cadence inside Run.
func (s *Server) refreshWorkBuddyLoop(ctx context.Context, window time.Duration) {
	for _, a := range s.Pool.Accounts() {
		if ctx.Err() != nil {
			return
		}
		if a.Provider != "workbuddy" || a.RefreshToken == "" || !s.Pool.Enabled(a.ID) {
			continue
		}
		exp := workbuddy.AccessTokenExpiry(a)
		if exp.IsZero() || time.Until(exp) > window {
			continue
		}
		if _, _, err := s.refreshAccountTokens(ctx, a); err != nil {
			slog.Warn("workbuddy token refresh failed", "account", a.ID, "error", err)
		}
	}
}
