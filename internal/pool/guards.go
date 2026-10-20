package pool

import (
	"strconv"
	"strings"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/provider"
)

// dailyKey returns the guard window key for now: the calendar date shifted by
// reset_hour so counters roll over exactly at the configured local hour
// (reset_hour=0 -> key is today; reset_hour=4 -> key flips at 04:00 local).
func dailyKey(now time.Time, resetHour int) string {
	shifted := now.Add(-time.Duration(resetHour) * time.Hour)
	return shifted.Format("2006-01-02")
}

// resetDaily rolls the guard counters over when the window key changes.
func (e *entry) resetDaily(now time.Time, resetHour int) {
	key := dailyKey(now, resetHour)
	if key == e.state.DailyReset {
		return
	}
	e.state.DailyReset = key
	if len(e.state.DailyTokens) > 64 {
		e.state.DailyTokens = map[string]int64{}
	} else {
		for k := range e.state.DailyTokens {
			delete(e.state.DailyTokens, k)
		}
	}
	e.state.DailyCredits = 0
}

// creditRate parses the catalog "credits" multiplier of a model
// ("x0.34" -> 0.34, "x0.00" -> 0 free). Unknown models are treated as paid
// with rate 1.0 so the daily-credits guard never lets an unbilled model
// through.
func creditRate(model provider.Model) float64 {
	raw := strings.TrimSpace(strings.ToLower(anyString(model["credits"])))
	switch raw {
	case "x0.00", "x0", "0", "0.00":
		return 0.0 // explicitly free
	case "":
		return 1.0 // unknown -> paid
	}
	if v, err := strconv.ParseFloat(strings.TrimPrefix(raw, "x"), 64); err == nil && v >= 0 && strings.HasPrefix(raw, "x") {
		return v
	}
	return 1.0
}

func anyString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}

// modelByID looks up the discovered model metadata for a model id.
func (e *entry) modelByID(model string) provider.Model {
	for _, m := range e.state.Models {
		if id, _ := m["id"].(string); id == model {
			return m
		}
	}
	return provider.Model{"credits": ""}
}

// estimatedCredits converts a token usage into the WorkBuddy credit estimate
// using the model's catalog multiplier (free models cost nothing).
func (e *entry) estimatedCredits(model string, tokens int64) float64 {
	if tokens <= 0 {
		return 0
	}
	return float64(tokens) * creditRate(e.modelByID(model))
}

// guardReason returns a rejection reason when a guard blocks this account for
// the model, or "" when the request may proceed.
func (e *entry) guardReason(model string, now time.Time, g config.Guards) string {
	e.resetDaily(now, g.ResetHour)
	if g.DailyTokens > 0 && e.state.DailyTokens[""] >= g.DailyTokens {
		return "daily_token_limit"
	}
	if model != "" && g.PerModelTokens > 0 && e.state.DailyTokens[model] >= g.PerModelTokens {
		return "per_model_daily_token_limit"
	}
	if model != "" && g.DailyCredits > 0 {
		if rate := creditRate(e.modelByID(model)); rate > 0 && e.state.DailyCredits >= g.DailyCredits {
			return "daily_credit_limit"
		}
	}
	return ""
}

// reserveLow reports whether a fresh quota snapshot has fallen below the
// reserve floor (account credits across all applicable pools).
func reserveLow(q *provider.Quota, model string, floor float64, now time.Time) bool {
	if floor <= 0 || q == nil {
		return false
	}
	if !QuotaFresh(q, now) {
		return false
	}
	if q.RemainingTokens != nil && float64(*q.RemainingTokens) < floor {
		return true
	}
	var total float64
	found := false
	for _, b := range q.Buckets {
		if len(b.Models) > 0 && (model == "" || !contains(b.Models, model)) {
			continue
		}
		total += b.Remaining
		found = true
	}
	if !found {
		return false
	}
	return total < floor
}

// guardBlockReason combines the daily guards with the reserve floor check.
func (e *entry) guardBlockReason(model string, now time.Time, g config.Guards) string {
	if reason := e.guardReason(model, now, g); reason != "" {
		return reason
	}
	if reserveLow(e.state.Quota, model, g.ReserveCredits, now) {
		return "reserve_credits"
	}
	return ""
}

// recordDaily accumulates a request's usage into the guard counters.
func (e *entry) recordDaily(model string, tokens int64, credits float64, resetHour int) {
	if tokens <= 0 {
		return
	}
	e.resetDaily(time.Now(), resetHour)
	e.state.DailyTokens[""] += tokens
	if model != "" {
		e.state.DailyTokens[model] += tokens
	}
	e.state.DailyCredits += credits
}
