package pool

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/provider"
)

func (p *Pool) Discovery(id string, models []provider.Model, err error, cooldown time.Duration) {
	p.discovery(id, "", "", models, err, cooldown)
}
func (p *Pool) DiscoveryFor(a config.Account, models []provider.Model, err error, cooldown time.Duration) {
	p.discovery(a.ID, a.APIKey, a.BaseURL, models, err, cooldown)
}
func (p *Pool) discovery(id, key, baseURL string, models []provider.Model, err error, cooldown time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.notifyLocked()
	for _, e := range p.entries {
		if e.state.ID != id {
			continue
		}
		if key != "" && (e.config.APIKey != key || e.config.BaseURL != baseURL) {
			return
		}
		s := &e.state
		now := time.Now()
		s.LastCheckedAt = &now
		if err != nil {
			s.DiscoveryError = err.Error()
			var he *provider.HTTPError
			if errors.As(err, &he) && he.Status == 401 {
				s.CredentialStatus = "invalid"
			}
			if cooldown > 0 {
				until := time.Now().Add(cooldown)
				if until.After(s.CooldownUntil) {
					s.CooldownUntil = until
				}
			}
			return
		}
		s.Models = models
		s.ModelsCheckedAt = &now
		s.DiscoveryError = ""
		s.CredentialStatus = "valid"
		for model, until := range e.blockedModels {
			if !now.Before(until) {
				delete(e.blockedModels, model)
			}
		}
		for model, until := range e.deniedModels {
			if !now.Before(until) {
				delete(e.deniedModels, model)
			}
		}
	}
}
func (p *Pool) Accounts() []config.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]config.Account, len(p.entries))
	for i, e := range p.entries {
		out[i] = e.config
	}
	return out
}
func (p *Pool) Snapshot() []State {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]State, 0, len(p.entries))
	for _, e := range p.entries {
		s := e.state
		s.ModelCooldowns = map[string]time.Time{}
		s.UnavailableModels = map[string]time.Time{}
		for model, until := range e.deniedModels {
			if time.Now().Before(until) {
				s.UnavailableModels[model] = until
			}
		}
		for model, until := range e.blockedModels {
			if time.Now().Before(until) {
				s.ModelCooldowns[model] = until
			}
		}
		s.QuotaStatus = "unsupported"
		if e.config.QuotaFeedURL != "" {
			s.QuotaStatus = "unknown"
		}
		if s.Quota != nil {
			s.QuotaStatus = "reported"
			if !QuotaFresh(s.Quota, time.Now()) {
				s.QuotaStatus = "stale"
			}
		}
		out = append(out, s)
	}
	// Deep copy protects nested maps/pointers from callers and encoding outside lock.
	b, _ := json.Marshal(out)
	var cloned []State
	_ = json.Unmarshal(b, &cloned)
	return cloned
}

func (p *Pool) ThrottleModel(id, model string, delay time.Duration) {
	if model == "" || delay <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.state.ID == id {
			until := time.Now().Add(delay)
			if until.After(e.blockedModels[model]) {
				e.blockedModels[model] = until
			}
			e.startRecovery(model, until)
			p.notifyLocked()
			return
		}
	}
}

func (p *Pool) DenyModel(id, model string, ttl time.Duration) {
	if model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.state.ID == id {
			if len(e.deniedModels) >= 256 {
				e.deniedModels = map[string]time.Time{}
			}
			e.deniedModels[model] = time.Now().Add(ttl)
			p.notifyLocked()
			return
		}
	}
}

// ModelList returns the aggregated model catalog of one provider inside one
// routing group: every fresh, enabled, valid account of that provider and
// group contributes its discovered models, deduplicated by model id.
// owned_by reports the provider. group == "" means the default group.
func (p *Pool) ModelList(prov, group string, ttl time.Duration) ([]provider.Model, bool) {
	if group == "" {
		group = DefaultGroup
	}
	return p.modelList(ttl, func(e *entry) bool { return e.group == group && e.config.Provider == prov })
}

// ModelListAll returns the aggregated model→account catalog across every
// registered platform and every routing group: the same model id advertised
// by accounts of different platforms is merged into a single entry whose
// platforms field lists each serving platform. This is the admin-side surface;
// client /v1/models is group-scoped.
func (p *Pool) ModelListAll(ttl time.Duration) ([]provider.Model, bool) {
	return p.modelList(ttl, func(e *entry) bool { return true })
}

// ModelListGroup returns the aggregated model→account catalog of one routing
// group across every platform: the routing surface for /v1/models when no
// explicit platform is selected. group == "" means the default group.
func (p *Pool) ModelListGroup(group string, ttl time.Duration) ([]provider.Model, bool) {
	if group == "" {
		group = DefaultGroup
	}
	return p.modelList(ttl, func(e *entry) bool { return e.group == group })
}

// GroupInfo describes one routing group for the admin console.
type GroupInfo struct {
	Name     string `json:"name"`
	Accounts int    `json:"accounts"`
	Enabled  int    `json:"enabled"`
}

// Groups lists every routing group with account counts in stable sorted
// order, so the admin surface can render the group chooser and per-group
// capacity without hard-coding names.
func (p *Pool) Groups() []GroupInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	counts := map[string]int{}
	enabled := map[string]int{}
	for _, e := range p.entries {
		g := e.group
		counts[g]++
		if e.state.Enabled {
			enabled[g]++
		}
	}
	names := make([]string, 0, len(counts))
	for g := range counts {
		names = append(names, g)
	}
	sort.Strings(names)
	out := make([]GroupInfo, 0, len(names))
	for _, g := range names {
		out = append(out, GroupInfo{Name: g, Accounts: counts[g], Enabled: enabled[g]})
	}
	return out
}

func (p *Pool) modelList(ttl time.Duration, include func(*entry) bool) ([]provider.Model, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	all := map[string]provider.Model{}
	platforms := map[string]map[string]bool{}
	discovered := false
	now := time.Now()
	for _, e := range p.entries {
		s := &e.state
		if !include(e) || !s.Enabled || s.CredentialStatus == "invalid" || s.ModelsCheckedAt == nil || now.Sub(*s.ModelsCheckedAt) > ttl {
			continue
		}
		discovered = true
		for _, m := range s.Models {
			id, _ := m["id"].(string)
			if !e.supports(id) {
				continue
			}
			if _, ok := all[id]; !ok {
				c := provider.Model{}
				for k, v := range m {
					c[k] = v
				}
				c["object"] = "model"
				c["owned_by"] = e.config.Provider
				all[id] = c
			}
			if platforms[id] == nil {
				platforms[id] = map[string]bool{}
			}
			platforms[id][e.config.Provider] = true
		}
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]provider.Model, 0, len(ids))
	for _, id := range ids {
		m := all[id]
		if len(platforms[id]) > 1 {
			ps := make([]string, 0, len(platforms[id]))
			for prov := range platforms[id] {
				ps = append(ps, prov)
			}
			sort.Strings(ps)
			m["platforms"] = ps
		}
		out = append(out, m)
	}
	return out, discovered
}
func (p *Pool) SetEnabled(id string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.state.ID == id {
			e.state.Enabled = enabled
			p.notifyLocked()
			return nil
		}
	}
	return os.ErrNotExist
}

// Enabled reports whether the account is currently enabled.
func (p *Pool) Enabled(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.state.ID == id {
			return e.state.Enabled
		}
	}
	return false
}

// SetCredentialValid lifts a 401-invalid marker after a successful OAuth
// token refresh, so the retry is schedulable again.
func (p *Pool) SetCredentialValid(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.state.ID == id {
			e.state.CredentialStatus = "valid"
			e.state.DiscoveryError = ""
			return
		}
	}
}

// SetWorkBuddyTask records a completed WorkBuddy daily-bonus claim. kind is
// "checkin" | "activity" | "daily_chat"; a non-empty date stamps the claim
// (wall-clock "2006-01-02"), and checkin additionally stores the CN check-in
// activity snapshot when info is non-nil. Unknown ids are os.ErrNotExist.
func (p *Pool) SetWorkBuddyTask(id, kind, date string, info *provider.CheckinStatus) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.config.ID == id {
			switch kind {
			case "checkin":
				if date != "" {
					e.state.CheckinDate = date
				}
				if info != nil {
					e.state.CheckinInfo = info
				}
			case "activity":
				if date != "" {
					e.state.ActivityReportDate = date
				}
			case "daily_chat":
				if date != "" {
					e.state.DailyChatDate = date
				}
			default:
				return errors.New("unknown workbuddy task kind")
			}
			p.notifyLocked()
			return nil
		}
	}
	return os.ErrNotExist
}
