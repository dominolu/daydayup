package gateway

// Usage insight (M3): per-client / per-model / per-account aggregation with an honest
// cost estimate. Costs use the configured price table (CNY per 1M tokens).
// WorkBuddy models without a token price fall back to their official credit
// multiplier (marked estimated); models with no price at all are counted as
// unpriced, never guessed.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// usageRow aggregates one dimension bucket.
type usageRow struct {
	Key        string  `json:"key"`
	Requests   int     `json:"requests"`
	Input      int64   `json:"input_tokens"`
	Output     int64   `json:"output_tokens"`
	Cached     int64   `json:"cached_input_tokens"`
	Total      int64   `json:"total_tokens"`
	Cost       float64 `json:"cost"`
	CostMode   string  `json:"cost_mode"` // "price" | "estimated" | "unpriced"
	Unpriced   int     `json:"unpriced_requests"`
}

// modelCost is the resolved cost recipe for one model id.
type modelCost struct {
	Input, Output, Cached float64 // CNY per 1M tokens; <=0 means not priced
	Multiplier            float64 // WorkBuddy credit multiplier (estimated mode)
	Configured            bool    // a pricing entry exists (zero price = free, still "price" mode)
}

// resolveCost builds the cost recipe for a model from the configured price
// table and, as a fallback, a WorkBuddy credit multiplier found on any
// account's discovered model metadata ("credits": "x9.3").
func (s *Server) resolveCost(model string) modelCost {
	var mc modelCost
	if p, ok := s.Config.Pricing[model]; ok {
		mc.Configured = true
		if p.Input != nil {
			mc.Input = *p.Input
		}
		if p.Output != nil {
			mc.Output = *p.Output
		}
		if p.CacheRead != nil {
			mc.Cached = *p.CacheRead
		}
		if p.CreditMultiplier != nil {
			mc.Multiplier = *p.CreditMultiplier
		}
		return mc
	}
	for _, st := range s.Pool.Snapshot() {
		for _, m := range st.Models {
			if m["id"] == model {
				if credits, ok := m["credits"].(string); ok && strings.HasPrefix(credits, "x") {
					if v, err := strconv.ParseFloat(strings.TrimPrefix(credits, "x"), 64); err == nil {
						mc.Multiplier = v
					}
				}
			}
		}
	}
	return mc
}

// usageCost computes the cost of one request given its usage (tokens in
// millions). Returns the cost amount and its mode.
func costFor(usage tokens, mc modelCost) (float64, string) {
	inM := float64(usage.Input-usage.Cached) / 1e6
	if inM < 0 {
		inM = 0
	}
	outM := float64(usage.Output) / 1e6
	cachedM := float64(usage.Cached) / 1e6
	if mc.Configured {
		return inM*mc.Input + outM*mc.Output + cachedM*mc.Cached, "price"
	}
	if mc.Multiplier > 0 {
		return (float64(usage.Input) + float64(usage.Output)) * mc.Multiplier, "estimated"
	}
	return 0, "unpriced"
}

type tokens struct{ Input, Output, Cached int64 }

type usageAgg struct {
	client map[string]*usageRow
	model  map[string]*usageRow
	account map[string]*usageRow
}

func (a *usageAgg) add(row *usageRow, u tokens, cost float64, mode string) {
	row.Requests++
	row.Input += u.Input
	row.Output += u.Output
	row.Cached += u.Cached
	row.Total += u.Input + u.Output
	row.Cost += cost
	if mode == "unpriced" {
		row.Unpriced++
		if row.CostMode == "" {
			row.CostMode = "unpriced"
		}
	} else if row.CostMode == "" || row.CostMode == "unpriced" {
		row.CostMode = mode
	}
}

// usageSummary builds the aggregated usage view for the last windowSeconds
// (0 = all time). Cost is resolved per record model with caching.
func (s *Server) usageSummary(windowSeconds int64) map[string]any {
	agg := &usageAgg{
		client:  map[string]*usageRow{},
		model:   map[string]*usageRow{},
		account: map[string]*usageRow{},
	}
	from := time.Time{}
	if windowSeconds > 0 {
		from = time.Now().Add(-time.Duration(windowSeconds) * time.Second)
	}
	costCache := map[string]modelCost{}
	unpriced, estimated, priced := 0, 0, 0
	if s.Sessions != nil {
		for _, r := range s.Sessions.RecordsSince(from) {
			u := tokens{Input: r.Usage.Input, Output: r.Usage.Output, Cached: r.Usage.Cached}
			if u.Input == 0 && u.Output == 0 {
				continue
			}
			mc, ok := costCache[r.Model]
			if !ok {
				mc = s.resolveCost(r.Model)
				costCache[r.Model] = mc
			}
			cost, mode := costFor(u, mc)
			switch mode {
			case "price":
				priced++
			case "estimated":
				estimated++
			default:
				unpriced++
			}
			key := r.Identity.Client
			if key == "" {
				key = "Unknown"
			}
			agg.add(rowOrNew(agg.client, key), u, cost, mode)
			if r.Model != "" {
				agg.add(rowOrNew(agg.model, r.Model), u, cost, mode)
			}
			acct := r.AccountID
			agg.add(rowOrNew(agg.account, acct), u, cost, mode)
		}
	} else {
		// Sessions disabled: the account dimension falls back to the pool's
		// own counters; clients and models are unknown in this mode.
		for _, st := range s.Pool.Snapshot() {
			row := &usageRow{Key: st.ID, CostMode: "unpriced"}
			row.Requests = int(st.Stats.Requests)
			row.Input = st.Stats.Input
			row.Output = st.Stats.Output
			row.Cached = st.Stats.Cached
			row.Total = st.Stats.Input + st.Stats.Output
			agg.account[row.Key] = row
		}
	}
	rows := map[string][]usageRow{
		"clients":  sortedRows(agg.client),
		"models":   sortedRows(agg.model),
		"accounts": sortedRows(agg.account),
	}
	return map[string]any{
		"rows":                    rows,
		"priced_requests":         priced,
		"estimated_requests":      estimated,
		"unpriced_requests":       unpriced,
		"currency":                "CNY",
		"cost_estimate_note":      "价格表来自配置 pricing（每 1M token 人民币）；无价格模型按上游积分倍率估算，两者皆无记为未计价。",
		"window_seconds":          windowSeconds,
		"session_storage_enabled": s.Sessions != nil,
		"price_table":             s.Config.Pricing,
	}
}

func rowOrNew(m map[string]*usageRow, key string) *usageRow {
	r, ok := m[key]
	if !ok {
		r = &usageRow{Key: key}
		m[key] = r
	}
	return r
}

func sortedRows(m map[string]*usageRow) []usageRow {
	out := make([]usageRow, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return out
}

// usageAdmin handles GET /admin/v1/usage and GET /admin/v1/routing.
func (s *Server) usageAdmin(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/admin/v1/usage" && r.Method == http.MethodGet {
		window := r.URL.Query().Get("window")
		var seconds int64
		switch window {
		case "24h":
			seconds = 86400
		case "7d":
			seconds = 7 * 86400
		}
		writeJSON(w, 200, s.usageSummary(seconds))
		return
	}
	if path == "/admin/v1/routing" && r.Method == http.MethodGet {
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		writeJSON(w, 200, s.routingSnapshot(limit))
		return
	}
	apiError(w, 404, "not_found", "admin endpoint not found")
}

