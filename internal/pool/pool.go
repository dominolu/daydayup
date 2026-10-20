// Package pool schedules requests across gateway accounts: reservation,
// recovery, discovery, quota observation and durable state checkpoints.
package pool

import (
	"errors"
	"os"
	"sync"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/provider"
	"daydayup/internal/store"
)

type Usage struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Total  int64 `json:"total_tokens"`
	Cached int64 `json:"cached_input_tokens"`
	Known  bool  `json:"known"`
}
type Stats struct {
	Requests     uint64     `json:"requests"`
	Failures     uint64     `json:"failures"`
	UnknownUsage uint64     `json:"requests_without_usage"`
	Input        int64      `json:"input_tokens"`
	Output       int64      `json:"output_tokens"`
	Total        int64      `json:"total_tokens"`
	Cached       int64      `json:"cached_input_tokens"`
	LastStatus   int        `json:"last_status"`
	LastRequest  *time.Time `json:"last_request,omitempty"`
}
type State struct {
	ID                string               `json:"id"`
	Name              string               `json:"name"`
	BaseURL           string               `json:"base_url"`
	KeySource         string               `json:"key_source"`
	AccountSource     string               `json:"account_source"`
	ModelsAllowlist   []string             `json:"models_allowlist,omitempty"`
	Provider          string               `json:"provider"`
	Group             string               `json:"group,omitempty"`
	Realm             string               `json:"realm,omitempty"`
	Product           string               `json:"product,omitempty"`
	Enabled           bool                 `json:"enabled"`
	CredentialStatus  string               `json:"credential_status"`
	InFlight          int                  `json:"in_flight"`
	MaxInFlight       int                  `json:"max_in_flight"`
	CooldownUntil     time.Time            `json:"cooldown_until"`
	ModelCooldowns    map[string]time.Time `json:"model_cooldowns,omitempty"`
	UnavailableModels map[string]time.Time `json:"unavailable_models,omitempty"`
	Models            []provider.Model     `json:"models"`
	ModelsCheckedAt   *time.Time           `json:"models_checked_at,omitempty"`
	LastCheckedAt     *time.Time           `json:"last_checked_at,omitempty"`
	DiscoveryError    string               `json:"discovery_error,omitempty"`
	QuotaStatus       string               `json:"quota_status"`
	QuotaSyncError    string               `json:"quota_sync_error,omitempty"`
	Quota             *provider.Quota      `json:"quota"`
	Stats             Stats                `json:"stats"`
	// QuotaExhaustedUntil parks an account after a definitive upstream
	// out-of-credits answer (WorkBuddy 402) until the reset wall clock.
	QuotaExhaustedUntil *time.Time `json:"quota_exhausted_until,omitempty"`
	// Daily guard counters. DailyReset is the window key ("2006-01-02") the
	// counters belong to; a new key rolls the counters over at reset_hour.
	DailyReset   string           `json:"daily_reset,omitempty"`
	DailyTokens  map[string]int64 `json:"daily_tokens,omitempty"`
	DailyCredits float64          `json:"daily_credits,omitempty"`
	// WorkBuddy daily-bonus claims. The *_date fields are wall-clock keys
	// ("2006-01-02"); empty means never claimed, a past key means due again
	// today. CheckinInfo is the CN check-in activity snapshot for the UI.
	CheckinDate        string                  `json:"wb_checkin_date,omitempty"`
	ActivityReportDate string                  `json:"wb_activity_report_date,omitempty"`
	DailyChatDate      string                  `json:"wb_daily_chat_date,omitempty"`
	CheckinInfo        *provider.CheckinStatus `json:"wb_checkin_info,omitempty"`
}
type entry struct {
	config         config.Account
	state          State
	group          string // normalized routing group ("" is never stored)
	blockedModels  map[string]time.Time
	deniedModels   map[string]time.Time
	inFlightModels map[string]int
	recovery       map[string]recoveryWindow
	quotaCheckedAt time.Time
}

// Routing modes for the account selection order.
const (
	RoutingRoundRobin = "round_robin"
	RoutingSmart      = "smart"
)

type Pool struct {
	mu      sync.Mutex
	saveMu  sync.Mutex
	entries []*entry
	cursor  map[string]int
	guards  config.Guards
	routing string
	changed chan struct{}
	// persistDB is the lazily opened shared database used by Save/Load.
	persistMu sync.Mutex
	persistDB *store.DB
}

func New(accounts []config.Account) *Pool {
	p := &Pool{cursor: map[string]int{}, changed: make(chan struct{}), routing: RoutingRoundRobin}
	for _, a := range accounts {
		p.entries = append(p.entries, newEntry(a))
	}
	return p
}

// SetGuards installs the daily usage guard configuration. Called once at
// gateway construction; the pool is the only reader of guard state.
func (p *Pool) SetGuards(g config.Guards) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.guards = g
}

// SetRouting installs the account selection order ("round_robin" or "smart").
// Called once at gateway construction.
func (p *Pool) SetRouting(mode string) {
	if mode != RoutingSmart {
		mode = RoutingRoundRobin
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routing = mode
}

// Routing returns the configured selection mode.
func (p *Pool) Routing() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.routing
}

func (p *Pool) Guards() config.Guards {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.guards
}

func newEntry(a config.Account) *entry {
	name := a.Name
	if name == "" {
		name = a.ID
	}
	source := "config"
	if a.Managed {
		source = "managed"
	}
	keySource := "environment"
	if a.APIKeyEnv == "" && a.Managed {
		keySource = "encrypted_store"
	}
	group := a.Group
	if group == "" {
		group = "default"
	}
	return &entry{config: a, group: group, state: State{ID: a.ID, Name: name, BaseURL: a.BaseURL, KeySource: keySource, AccountSource: source, ModelsAllowlist: a.Models, Provider: a.Provider, Group: group, Realm: a.Realm, Product: a.Product, Enabled: !a.Disabled, CredentialStatus: "unknown", MaxInFlight: a.MaxInFlight, Models: []provider.Model{}, DailyTokens: map[string]int64{}}, blockedModels: map[string]time.Time{}, deniedModels: map[string]time.Time{}, inFlightModels: map[string]int{}, recovery: map[string]recoveryWindow{}}
}

// Realm returns the scheduling realm of the account (used by WorkBuddy
// realm-restriction holds and realm preference).
func Realm(a config.Account) string {
	if a.Provider == "workbuddy" {
		if a.Realm == "cn" || a.Realm == "intl" {
			return a.Realm
		}
		return "intl"
	}
	return ""
}

var ErrBusy = errors.New("account has active requests")
var ErrDuplicate = errors.New("account id or upstream credential already exists")

func (p *Pool) Account(id string) (config.Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.config.ID == id {
			a := e.config
			a.Models = append([]string(nil), a.Models...)
			return a, nil
		}
	}
	return config.Account{}, os.ErrNotExist
}

// Persist while holding the reservation lock: a failed disk write cannot leave
// an account live in memory, and active requests prevent replacement/removal.
func (p *Pool) Upsert(a config.Account, create bool, persist func(config.Account) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	index := -1
	for i, e := range p.entries {
		if e.config.ID == a.ID {
			index = i
		} else if e.config.Provider == a.Provider && e.config.BaseURL == a.BaseURL && e.config.APIKey == a.APIKey {
			return ErrDuplicate
		}
	}
	if create && index >= 0 {
		return ErrDuplicate
	}
	if !create && index < 0 {
		return os.ErrNotExist
	}
	if index >= 0 && p.entries[index].state.InFlight > 0 {
		return ErrBusy
	}
	a.Managed = true
	if index >= 0 {
		a.Disabled = !p.entries[index].state.Enabled
	}
	if err := persist(a); err != nil {
		return err
	}
	next := newEntry(a)
	if index >= 0 {
		old := p.entries[index]
		next.state.Stats = old.state.Stats
		next.state.Enabled = old.state.Enabled
		if old.config.APIKey == a.APIKey && old.config.BaseURL == a.BaseURL {
			next.state.CredentialStatus = old.state.CredentialStatus
			next.state.Models = old.state.Models
			next.state.ModelsCheckedAt = old.state.ModelsCheckedAt
			next.state.LastCheckedAt = old.state.LastCheckedAt
			next.state.DiscoveryError = old.state.DiscoveryError
			next.state.Quota = old.state.Quota
			next.state.CooldownUntil = old.state.CooldownUntil
			next.blockedModels = old.blockedModels
			next.deniedModels = old.deniedModels
			next.recovery = old.recovery
		}
		p.entries[index] = next
	} else {
		p.entries = append(p.entries, next)
	}
	p.notifyLocked()
	return nil
}
func (p *Pool) Remove(id string, persist func(string) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.config.ID == id {
			if e.state.InFlight > 0 {
				return ErrBusy
			}
			if err := persist(id); err != nil {
				return err
			}
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			p.cursor = map[string]int{}
			p.notifyLocked()
			return nil
		}
	}
	return os.ErrNotExist
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
