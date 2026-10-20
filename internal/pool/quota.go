package pool

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/provider"
)

func QuotaFresh(q *provider.Quota, now time.Time) bool {
	if q == nil || !now.Before(q.ValidUntil) {
		return false
	}
	for _, b := range q.Buckets {
		if b.ResetsAt != nil && !now.Before(*b.ResetsAt) {
			return false
		}
	}
	return true
}

func quotaPositive(q *provider.Quota) bool {
	if q == nil {
		return false
	}
	if q.RemainingTokens != nil && *q.RemainingTokens > 0 {
		return true
	}
	for _, b := range q.Buckets {
		if b.Remaining > 0 {
			return true
		}
	}
	return false
}
func exhausted(q *provider.Quota, model string, now time.Time) bool {
	if !QuotaFresh(q, now) {
		return false
	}
	if q.RemainingTokens != nil && *q.RemainingTokens <= 0 {
		return true
	}
	if q.Partial {
		return false
	}
	if model == "" {
		return false
	}
	// Within a pool every rolling window must have credit; alternative pools
	// are ORed (Flash-Lite can fall back from its dedicated pool to general).
	groups := map[string]bool{}
	for _, b := range q.Buckets {
		if len(b.Models) > 0 && !contains(b.Models, model) {
			continue
		}
		if _, ok := groups[b.Pool]; !ok {
			groups[b.Pool] = true
		}
		if b.Remaining <= 0 {
			groups[b.Pool] = false
		}
	}
	if len(groups) == 0 {
		return false
	}
	for _, available := range groups {
		if available {
			return false
		}
	}
	return true
}

func (p *Pool) SetQuota(id string, q *provider.Quota) error {
	if q != nil {
		now := time.Now()
		if q.Source == "" || q.ObservedAt.IsZero() || q.ObservedAt.After(now.Add(time.Minute)) || !q.ValidUntil.After(q.ObservedAt) || !q.ValidUntil.After(now) {
			return errors.New("quota requires source, observed_at, and a future valid_until")
		}
		if q.RemainingTokens != nil && *q.RemainingTokens < 0 {
			return errors.New("remaining_tokens must be nonnegative")
		}
		for _, b := range q.Buckets {
			if b.Name == "" || b.Pool == "" || b.Unit == "" || b.Remaining < 0 || b.Limit < 0 || (b.Limit > 0 && b.Remaining > b.Limit) {
				return errors.New("invalid quota bucket")
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.state.ID == id {
			if q != nil && e.state.Quota != nil && q.ObservedAt.Before(e.state.Quota.ObservedAt) {
				return errors.New("quota observation is older than current snapshot")
			}
			if q == nil {
				e.state.Quota = nil
			} else {
				b, _ := json.Marshal(q)
				var owned provider.Quota
				_ = json.Unmarshal(b, &owned)
				e.state.Quota = &owned
				if quotaPositive(&owned) {
					e.state.QuotaExhaustedUntil = nil
				}
			}
			e.state.QuotaSyncError = ""
			p.notifyLocked()
			return nil
		}
	}
	return os.ErrNotExist
}

func (p *Pool) QuotaError(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.state.ID == id {
			e.quotaCheckedAt = time.Now()
			if err == nil {
				e.state.QuotaSyncError = ""
			} else {
				e.state.QuotaSyncError = err.Error()
			}
			return
		}
	}
}

// QuotaRefreshDue reports whether the account quota should be re-fetched:
// when no snapshot exists and the last attempt is older than minInterval
// (so persistent failures do not hammer the billing endpoint), or when the
// current snapshot is older than maxAge.
func (p *Pool) QuotaRefreshDue(id string, minInterval, maxAge time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.state.ID == id {
			now := time.Now()
			if e.state.Quota != nil {
				return now.Sub(e.state.Quota.ObservedAt) > maxAge
			}
			if e.quotaCheckedAt.IsZero() {
				return true
			}
			return now.Sub(e.quotaCheckedAt) > minInterval
		}
	}
	return true
}

// MarkQuotaExhausted parks an account after a definitive upstream
// out-of-credits answer (WorkBuddy 402) until the reset wall clock.
func (p *Pool) MarkQuotaExhausted(id string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.state.ID == id {
			if e.state.QuotaExhaustedUntil == nil || until.After(*e.state.QuotaExhaustedUntil) {
				e.state.QuotaExhaustedUntil = &until
			}
			p.notifyLocked()
			return
		}
	}
}

// HoldRealm cools every account of a provider+realm (WorkBuddy abuse
// restriction 11140 implicates the whole realm through the derived device).
func (p *Pool) HoldRealm(prov, realm string, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	until := time.Now().Add(ttl)
	for _, e := range p.entries {
		if e.config.Provider == prov && Realm(e.config) == realm {
			if until.After(e.state.CooldownUntil) {
				e.state.CooldownUntil = until
			}
		}
	}
	p.notifyLocked()
}

// UpdateCredentials swaps account credentials in place (OAuth token refresh)
// without touching scheduling state or requiring an idle account: in-flight
// requests simply keep the token they already captured.
func (p *Pool) UpdateCredentials(a config.Account, persist func(config.Account) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.config.ID == a.ID {
			if e.config.Provider != a.Provider {
				return errors.New("provider mismatch on credential update")
			}
			if err := persist(a); err != nil {
				return err
			}
			old := e.config
			e.config = a
			// A successful OAuth refresh proves the credential works; lift any
			// 401-invalid marker so the caller can retry immediately.
			if old.APIKey != a.APIKey {
				e.state.CredentialStatus = "valid"
				e.state.DiscoveryError = ""
			}
			p.notifyLocked()
			return nil
		}
	}
	return os.ErrNotExist
}
