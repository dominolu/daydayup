package pool

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const ReasonModelDenied = "model_not_in_token_plan"

type Rejection struct {
	AccountID string
	Reason    string
	RetryAt   time.Time
}

// AcquireError describes the same atomic scheduling decision that rejected the
// request. Account IDs are available for internal logs; Error only gives counts.
type AcquireError struct {
	Provider   string
	Group      string
	Rejections []Rejection
	Changed    <-chan struct{}
}

// WaitHint only waits for transient scheduling constraints. Permanent account,
// model and quota failures keep their existing immediate error responses.
func (e *AcquireError) WaitHint() (time.Time, bool) {
	var next time.Time
	waitable := false
	for _, r := range e.Rejections {
		switch r.Reason {
		case "account_cooldown", "model_cooldown", "concurrency_limit", "rate_limit_recovery":
			waitable = true
			if !r.RetryAt.IsZero() && (next.IsZero() || r.RetryAt.Before(next)) {
				next = r.RetryAt
			}
		}
	}
	return next, waitable
}

func (e *AcquireError) OnlyReason(reason string) bool {
	if len(e.Rejections) == 0 {
		return false
	}
	for _, rejection := range e.Rejections {
		if rejection.Reason != reason {
			return false
		}
	}
	return true
}

func (e *AcquireError) Error() string {
	if len(e.Rejections) == 0 {
		if e.Provider == "" {
			if e.Group == "" {
				return "no account across any platform serves this model"
			}
			return fmt.Sprintf("no account in group %q serves this model", e.Group)
		}
		return fmt.Sprintf("no accounts configured for provider %q", e.Provider)
	}
	counts := map[string]int{}
	for _, rejection := range e.Rejections {
		counts[rejection.Reason]++
	}
	var reasons []string
	for reason, count := range counts {
		reasons = append(reasons, fmt.Sprintf("%s=%d", reason, count))
	}
	sort.Strings(reasons)
	return "no eligible account: " + strings.Join(reasons, ", ")
}
