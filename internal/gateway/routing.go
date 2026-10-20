package gateway

// Routing visualization (M3): a bounded in-memory ring of recent routing
// decisions ("谁先谁后、为什么") fed from the proxy attempt log, plus the
// live wait/cooldown state surfaced on the admin Routing page. Nothing here
// persists: the encrypted session archive remains the durable record.

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// routeHop is one upstream attempt inside a request.
type routeHop struct {
	Account   string    `json:"account"`
	Status    int       `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	Retry     bool      `json:"retry"`
	StartedAt time.Time `json:"started_at"`
}

// routeDecision is the per-request routing summary shown on the Routing page.
type routeDecision struct {
	RequestID string     `json:"request_id"`
	StartedAt time.Time  `json:"started_at"`
	Path      string     `json:"path"`
	Model     string     `json:"model"`
	Provider  string     `json:"provider"`
	Group     string     `json:"group,omitempty"`
	Status    int        `json:"status"`
	PoolWait  int64      `json:"pool_wait_ms"`
	PoolWaits int        `json:"pool_waits"`
	Hops      []routeHop `json:"hops"`
	Failure   string     `json:"failure,omitempty"`
}

type routeLog struct {
	mu  sync.Mutex
	buf []routeDecision
	max int
}

func newRouteLog(max int) *routeLog {
	if max <= 0 {
		max = 512
	}
	return &routeLog{max: max}
}

func (r *routeLog) push(d routeDecision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, d)
	if len(r.buf) > r.max {
		r.buf = append([]routeDecision{}, r.buf[len(r.buf)-r.max:]...)
	}
}

// snapshot returns the newest n decisions in reverse chronological order.
func (r *routeLog) snapshot(n int) []routeDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.buf) {
		n = len(r.buf)
	}
	out := make([]routeDecision, 0, n)
	for i := len(r.buf) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, r.buf[i])
	}
	return out
}

// waitActive counts requests currently parked in the shared cooldown wait
// queue (see acquireWaitingAccount).
func (s *Server) waitActiveCount() int64 { return s.waitActive.Load() }

// addRouteHop records one proxy attempt into the trace's hop chain.
func (t *requestTrace) addRouteHop(status int, retry bool, started time.Time) {
	t.hops = append(t.hops, routeHop{Account: t.AccountID, Status: status, Reason: t.Failure.Reason, Retry: retry, StartedAt: started})
}

// recordRoute pushes the completed request into the routing ring (called from
// logCompletion so admin traffic itself stays out of the ring).
func (s *Server) recordRoute(r *http.Request, status int, trace *requestTrace) {
	if s.routeLog == nil || trace == nil || trace.ID == "" {
		return
	}
	// Admin, metrics and health checks are gateway traffic, not routes: keep
	// them out of the decision timeline.
	if strings.HasPrefix(r.URL.Path, "/admin/") || r.URL.Path == "/metrics" || r.URL.Path == "/healthz" {
		return
	}
	d := routeDecision{
		RequestID: trace.ID,
		StartedAt: trace.Started,
		Path:      r.URL.Path,
		Model:     trace.Model,
		Provider:  trace.Provider,
		Group:     trace.Group,
		Status:    status,
		PoolWait:  trace.PoolWaitMS,
		PoolWaits: trace.PoolWaits,
		Hops:      append([]routeHop{}, trace.hops...),
		Failure:   trace.Failure.Reason,
	}
	s.routeLog.push(d)
}

func (s *Server) routingSnapshot(limit int) map[string]any {
	out := map[string]any{
		"decisions":   s.routeLog.snapshot(limit),
		"wait_active": s.waitActiveCount(),
	}
	// Live cooldowns from the pool: accounts cooling (with recovery time) and
	// per-model cooldowns, so the page shows "正在等待哪个账号、预计恢复时间".
	cooldowns := []map[string]any{}
	modelCooldowns := map[string][]map[string]any{}
	for _, a := range s.Pool.Snapshot() {
		if time.Now().Before(a.CooldownUntil) {
			cooldowns = append(cooldowns, map[string]any{"account": a.ID, "provider": a.Provider, "until": a.CooldownUntil})
		}
		for m, until := range a.ModelCooldowns {
			if time.Now().Before(until) {
				modelCooldowns[m] = append(modelCooldowns[m], map[string]any{"account": a.ID, "until": until})
			}
		}
	}
	out["cooldowns"] = cooldowns
	out["model_cooldowns"] = modelCooldowns
	return out
}
