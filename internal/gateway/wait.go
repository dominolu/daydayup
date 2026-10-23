package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/pool"
)

var errWaitQueueFull = errors.New("gateway waiting queue is full; retry later")

// errPoolWaitTimeout means the request spent its dedicated waiting budget
// without upstream capacity becoming available. The caller reports it as a
// fast 503 instead of letting rate-limit waiting consume the generation window.
var errPoolWaitTimeout = errors.New("no upstream capacity within the pool wait budget")

// poolWaitBudget returns the cumulative waiting allowance for one request.
// Zero means the config never enabled the separate budget.
func (s *Server) poolWaitBudget() time.Duration {
	if s.Config.PoolWaitTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(s.Config.PoolWaitTimeoutSeconds) * time.Second
}

// Each rejected selection carries a notification channel captured under the
// reservation lock, preventing a lost wakeup when an in-flight request ends.
func (s *Server) acquireWaitingAccount(ctx context.Context, r *http.Request, prov, group, model string, excluded map[string]bool) (config.Account, error) {
	t := traceFrom(r)
	var waitingSince time.Time
	budget := s.poolWaitBudget()
	defer func() {
		if !waitingSince.IsZero() {
			<-s.waitSlots
			s.waitActive.Add(-1)
			t.PoolWaitMS += time.Since(waitingSince).Milliseconds()
			s.logRecord(r, "pool_wait_finished", 0, time.Since(waitingSince), diagnostic{})
		}
	}()
	// used reports the waiting time already spent by earlier attempts plus the
	// current wait, so the budget spans the whole request rather than one call.
	used := func() time.Duration {
		spent := time.Duration(t.PoolWaitMS) * time.Millisecond
		if !waitingSince.IsZero() {
			spent += time.Since(waitingSince)
		}
		return spent
	}
	for {
		if err := ctx.Err(); err != nil {
			return config.Account{}, err
		}
		a, err := s.acquireSessionAccount(t, prov, group, model, excluded)
		if err == nil {
			return a, nil
		}
		var selection *pool.AcquireError
		if !errors.As(err, &selection) {
			return a, err
		}
		t.rejections = selection.Rejections
		next, waitable := selection.WaitHint()
		if !waitable {
			return a, err
		}
		// Reject before opening a waiting slot when earlier attempts already
		// spent the budget; otherwise every extra attempt would register a
		// zero-length wait in the archive.
		if budget > 0 && used() >= budget {
			return config.Account{}, errPoolWaitTimeout
		}
		if waitingSince.IsZero() {
			select {
			case s.waitSlots <- struct{}{}:
				waitingSince = time.Now()
				s.waitActive.Add(1)
				t.PoolWaits++
				s.logRecord(r, "pool_wait_started", 0, 0, diagnostic{}, "wait_reason", selection.Error(), "retry_at", next)
			case <-ctx.Done():
				return config.Account{}, ctx.Err()
			default:
				return config.Account{}, errWaitQueueFull
			}
		}
		// Waiting never resets the budget: a recovered account later than the
		// budget is not worth blocking the client for.
		remaining := time.Duration(0)
		if budget > 0 {
			remaining = budget - used()
			if remaining <= 0 {
				return config.Account{}, errPoolWaitTimeout
			}
		}
		var wake <-chan time.Time
		var timer *time.Timer
		delay := time.Duration(0)
		if !next.IsZero() {
			delay = time.Until(next)
		}
		if budget > 0 {
			// Wake at the budget boundary even when retry_at is later, so the
			// request fails fast instead of sleeping until the cooldown ends.
			if delay <= 0 || remaining < delay {
				delay = remaining
			}
		}
		if delay > 0 {
			if delay < time.Millisecond {
				delay = time.Millisecond
			}
			timer = time.NewTimer(delay)
			wake = timer.C
		}
		select {
		case <-ctx.Done():
		case <-selection.Changed:
		case <-wake:
		}
		if timer != nil {
			timer.Stop()
		}
		if budget > 0 && used() >= budget {
			return config.Account{}, errPoolWaitTimeout
		}
	}
}

func proxyContextError(w http.ResponseWriter, r *http.Request, waiting bool) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		apiError(w, 499, "client_cancelled", "client cancelled the request")
		traceFrom(r).Failure.Layer = "client"
		return
	}
	code, message := "upstream_timeout", "request deadline exceeded"
	if waiting {
		code, message = "pool_wait_timeout", "request deadline exceeded while waiting for upstream capacity or rate-limit recovery"
	}
	apiError(w, http.StatusGatewayTimeout, code, message)
}

func rateLimitDelay(retryAfter string, rejection int) time.Duration {
	fallback := 30 * time.Second
	for i := 1; i < rejection && fallback < 2*time.Minute; i++ {
		fallback *= 2
	}
	delay := retryDelay(retryAfter, fallback)
	// Retry-After: 0 must not turn an unlimited retry budget into a busy loop.
	if delay < time.Second {
		delay = time.Second
	}
	return delay
}
