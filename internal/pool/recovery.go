package pool

import "time"

// For one minute after a rate-limit cooldown, admit a single request at a time,
// at most once per second. This is recovery pacing, not an inferred TPM/RPM.
type recoveryWindow struct {
	Until time.Time
	Next  time.Time
}

func (p *Pool) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (e *entry) startRecovery(scope string, cooldownEnd time.Time) {
	w := e.recovery[scope]
	if until := cooldownEnd.Add(time.Minute); until.After(w.Until) {
		w.Until = until
	}
	if cooldownEnd.After(w.Next) {
		w.Next = cooldownEnd
	}
	e.recovery[scope] = w
}

func (e *entry) recoveryWait(model string, now time.Time) (time.Time, bool) {
	for _, scope := range []string{"", model} {
		w := e.recovery[scope]
		if !now.Before(w.Until) {
			continue
		}
		inFlight := e.inFlightModels[scope]
		if scope == "" {
			inFlight = e.state.InFlight
		}
		if inFlight > 0 {
			return w.Until, true // Finish signals earlier when the probe completes.
		}
		if now.Before(w.Next) {
			return w.Next, true
		}
	}
	return time.Time{}, false
}

func (e *entry) reserveRecovery(model string, now time.Time) {
	for scope, w := range e.recovery {
		if !now.Before(w.Until) {
			delete(e.recovery, scope)
		} else if scope == "" || scope == model {
			w.Next = now.Add(time.Second)
			e.recovery[scope] = w
		}
	}
}
