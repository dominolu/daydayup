package pool

import (
	"net/http"
	"time"

	"daydayup/internal/config"
)

func (e *entry) supports(model string) bool {
	if model == "" {
		return true
	}
	if time.Now().Before(e.deniedModels[model]) {
		return false
	}
	if len(e.config.Models) > 0 && !contains(e.config.Models, model) {
		return false
	}
	if e.state.ModelsCheckedAt != nil {
		for _, m := range e.state.Models {
			if m["id"] == model {
				return true
			}
		}
		return false
	}
	return true
}

// Acquire reserves capacity atomically and rotates across account identities.
// The default routing group ("default") is used.
func (p *Pool) Acquire(prov, model string, excluded map[string]bool) (config.Account, error) {
	return p.AcquirePreferred(prov, model, excluded, "", false)
}

// Strict binding protects requests referring to account-owned upstream state.
func (p *Pool) AcquirePreferred(prov, model string, excluded map[string]bool, preferred string, strict bool) (config.Account, error) {
	return p.acquire(prov, "", DefaultGroup, model, excluded, preferred, strict)
}

// AcquirePreferredRealm prefers accounts of the given WorkBuddy realm
// (intl/cn) before falling back to the whole pool, so a request bound to one
// regional exit never randomly mixes fingerprints between the two regions.
//
// prov selects a single platform: an empty provider means the account pool is
// the aggregated model→account mapping across every registered platform (the
// same model id served by any platform's account is a candidate). Explicit
// platforms still work as a filter for clients that pin an identity.
func (p *Pool) AcquirePreferredRealm(prov, realm, model string, excluded map[string]bool, preferred string, strict bool) (config.Account, error) {
	return p.acquire(prov, realm, DefaultGroup, model, excluded, preferred, strict)
}

// AcquireGroup routes inside one routing group: the aggregated model→account
// mapping is bounded to the accounts whose group equals group. This is the
// "分组聚合路由" surface — same model, cross-platform, but never leaving the
// selected group. group is normalized by the caller (config.NormalizeGroup);
// the empty string means the default group.
func (p *Pool) AcquireGroup(prov, realm, group, model string, excluded map[string]bool, preferred string, strict bool) (config.Account, error) {
	return p.acquire(prov, realm, group, model, excluded, preferred, strict)
}

// DefaultGroup is the routing group of accounts that declare no group.
const DefaultGroup = "default"

// acquire is the round-robin reservation core with optional realm preference.
// The scheduling domain is (group, model): the accounts of one routing group
// across all platforms that support the model are the candidates, and the
// round-robin cursor rotates per (group, model) pair. With Routing=smart the
// first eligible candidate is replaced by the one whose quota window renews
// soonest (Magpie "smart": minimize what is lost at the next window reset).
// The ordering is stable and never prefers an account that would otherwise be
// skipped.
func (p *Pool) acquire(prov, realm, group, model string, excluded map[string]bool, preferred string, strict bool) (config.Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.entries)
	now := time.Now()
	if group == "" {
		group = DefaultGroup
	}
	cursorKey := group + "\x00" + model
	start := p.cursor[cursorKey]
	unavailable := &AcquireError{Provider: prov, Group: group, Changed: p.changed}
	order := make([]int, 0, n)
	seen := func(j int) bool {
		for _, o := range order {
			if o == j {
				return true
			}
		}
		return false
	}
	if preferred != "" {
		for j, e := range p.entries {
			if e.config.ID == preferred {
				order = append(order, j)
				break
			}
		}
	}
	// pass1Size marks how many order entries belong to the preferred WorkBuddy
	// realm; smart reordering never crosses that boundary into another realm
	// (fingerprints must not mix regions). Neutral platforms (no realm) may
	// still participate after the preferred realm's accounts.
	pass1Size := 0
	if !strict {
		if realm == "" {
			for i := 0; i < n; i++ {
				j := (start + i) % n
				if !seen(j) && p.entries[j].config.ID != preferred {
					order = append(order, j)
				}
			}
		} else {
			// Pass 1: the preferred WorkBuddy realm. Pass 2: neutral
			// platforms (accounts without a realm concept). Pass 3: the
			// other WorkBuddy realm, last so cross-region requests never prefer it.
			for i := 0; i < n; i++ {
				j := (start + i) % n
				e := p.entries[j]
				if !seen(j) && e.config.ID != preferred && Realm(e.config) == realm {
					order = append(order, j)
				}
			}
			pass1Size = len(order)
			for i := 0; i < n; i++ {
				j := (start + i) % n
				e := p.entries[j]
				if !seen(j) && e.config.ID != preferred && e.config.Provider != "workbuddy" {
					order = append(order, j)
				}
			}
			for i := 0; i < n; i++ {
				j := (start + i) % n
				e := p.entries[j]
				if !seen(j) && e.config.ID != preferred && e.config.Provider == "workbuddy" && Realm(e.config) != realm {
					order = append(order, j)
				}
			}
		}
	}
	if strict && len(order) == 0 {
		unavailable.Rejections = append(unavailable.Rejections, Rejection{AccountID: preferred, Reason: "pinned_account_missing"})
	}
	guards := p.guards
	// Pass 1: collect every eligible candidate plus per-account rejections.
	// Accounts outside the routing group are not candidates at all (group
	// isolation): they are skipped like other-platform accounts and never
	// surface a rejection.
	eligible := make([]int, 0, len(order))
	pass1End := -1
	for oi, j := range order {
		e := p.entries[j]
		s := &e.state
		if e.group != group {
			continue
		}
		if prov != "" && e.config.Provider != prov {
			continue
		}
		if oi == pass1Size && pass1End < 0 {
			pass1End = len(eligible)
		}
		reason := ""
		var retryAt time.Time
		switch {
		case excluded[s.ID]:
			reason = "already_attempted"
		case !s.Enabled:
			reason = "account_disabled"
		case s.CredentialStatus == "invalid":
			reason = "invalid_upstream_credential"
		case now.Before(e.deniedModels[model]):
			reason = ReasonModelDenied
		case !e.supports(model):
			reason = "model_not_supported"
		case e.state.QuotaExhaustedUntil != nil && now.Before(*e.state.QuotaExhaustedUntil):
			reason = "quota_exhausted"
		case exhausted(s.Quota, model, now):
			reason = "quota_exhausted"
		case now.Before(s.CooldownUntil):
			reason = "account_cooldown"
			retryAt = s.CooldownUntil
		case now.Before(e.blockedModels[model]):
			reason = "model_cooldown"
			retryAt = e.blockedModels[model]
		case s.InFlight >= s.MaxInFlight:
			reason = "concurrency_limit"
		}
		if reason == "" {
			if g := e.guardBlockReason(model, now, guards); g != "" {
				reason = g
			} else if at, blocked := e.recoveryWait(model, now); blocked {
				reason, retryAt = "rate_limit_recovery", at
			}
		}
		if reason != "" {
			unavailable.Rejections = append(unavailable.Rejections, Rejection{AccountID: s.ID, Reason: reason, RetryAt: retryAt})
			continue
		}
		eligible = append(eligible, j)
	}
	if len(eligible) == 0 {
		return config.Account{}, unavailable
	}
	// Pass 2: choose the reservation target.
	j := eligible[0]
	if p.routing == RoutingSmart && len(eligible) > 1 {
		// Smart reordering must respect the hard constraints that precede it:
		// a session-pinned account (preferred) always stays the target, and the
		// choice never leaves the preferred realm's pass.
		smartLimit := len(eligible)
		if preferred != "" {
			smartLimit = 1
		} else if realm != "" && pass1End > 0 {
			smartLimit = pass1End
		}
		// Smart ordering: prefer the eligible account whose fresh quota window
		// renews soonest (least time left to lose at the reset). When two
		// windows reset at the same time, the account with less remaining
		// credit is preferred (it has the least to waste). Candidates without
		// window information keep their round-robin order and never beat a
		// candidate that has window information.
		best := eligible[0]
		var bestKey time.Time
		var bestRemaining float64
		var bestHas bool
		for _, cand := range eligible[:smartLimit] {
			key, remaining, has := smartResetKey(p.entries[cand], now)
			if !has {
				continue
			}
			if !bestHas || key.Before(bestKey) || (key.Equal(bestKey) && remaining < bestRemaining) {
				best, bestKey, bestRemaining, bestHas = cand, key, remaining, true
			}
		}
		if bestHas {
			j = best
		}
	}
	e := p.entries[j]
	s := &e.state
	s.InFlight++
	e.inFlightModels[model]++
	e.reserveRecovery(model, now)
	s.Stats.Requests++
	s.Stats.LastRequest = &now
	p.cursor[cursorKey] = (j + 1) % n
	return e.config, nil
}

// smartResetKey returns the soonest future ResetsAt among the entries' fresh
// quota buckets, the remaining credit of that soonest window (used as the
// tie-break: same reset time → the account with less remaining wastes less),
// and whether any window information exists at all.
func smartResetKey(e *entry, now time.Time) (time.Time, float64, bool) {
	q := e.state.Quota
	if !QuotaFresh(q, now) {
		return time.Time{}, 0, false
	}
	var best time.Time
	var remaining float64
	found := false
	for _, b := range q.Buckets {
		if b.ResetsAt != nil && b.ResetsAt.After(now) {
			if !found || b.ResetsAt.Before(best) {
				best = *b.ResetsAt
				remaining = b.Remaining
				found = true
			}
		}
	}
	return best, remaining, found
}

// Finish never applies quota arithmetic to points: tokens and credits differ.
func (p *Pool) Finish(id string, status int, usage Usage, failed bool, cooldown time.Duration, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.state.ID != id {
			continue
		}
		s := &e.state
		if s.InFlight > 0 {
			s.InFlight--
		}
		if e.inFlightModels[model] > 1 {
			e.inFlightModels[model]--
		} else {
			delete(e.inFlightModels, model)
		}
		s.Stats.LastStatus = status
		if failed {
			s.Stats.Failures++
		}
		if usage.Known {
			s.Stats.Input += usage.Input
			s.Stats.Output += usage.Output
			s.Stats.Total += usage.Total
			s.Stats.Cached += usage.Cached
			e.recordDaily(model, usage.Total, e.estimatedCredits(model, usage.Total), p.guards.ResetHour)
		} else {
			s.Stats.UnknownUsage++
		}
		if status == 401 {
			s.CredentialStatus = "invalid"
		} else if status >= 200 && status < 300 {
			s.CredentialStatus = "valid"
		}
		if cooldown > 0 {
			until := time.Now().Add(cooldown)
			if until.After(s.CooldownUntil) {
				s.CooldownUntil = until
			}
			if status == http.StatusTooManyRequests {
				e.startRecovery("", until)
			}
		}
		if status == 404 && model != "" {
			if len(e.blockedModels) >= 256 {
				e.blockedModels = map[string]time.Time{}
			}
			e.blockedModels[model] = time.Now().Add(time.Minute)
		}
		p.notifyLocked()
	}
}
