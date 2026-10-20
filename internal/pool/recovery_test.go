package pool

import (
	"errors"
	"testing"
	"time"
)

func TestRecoverySerializesAndPacesOnlyLimitedModel(t *testing.T) {
	p := New(accounts()[:1])
	p.ThrottleModel("a", "m", time.Millisecond)
	_, err := p.Acquire("sensenova", "m", nil)
	var selection *AcquireError
	if !errors.As(err, &selection) {
		t.Fatal("missing cooldown rejection")
	}
	next, ok := selection.WaitHint()
	if !ok || next.IsZero() {
		t.Fatal("cooldown has no wakeup deadline")
	}
	time.Sleep(time.Until(next) + time.Millisecond)
	a, err := p.Acquire("sensenova", "m", nil)
	if err != nil {
		t.Fatal("first recovery probe blocked", err)
	}
	_, err = p.Acquire("sensenova", "m", nil)
	if !errors.As(err, &selection) || !selection.OnlyReason("rate_limit_recovery") {
		t.Fatal("simultaneous recovery probe admitted", err)
	}
	changed := selection.Changed
	p.Finish(a.ID, 200, Usage{}, false, 0, "m")
	select {
	case <-changed:
	default:
		t.Fatal("release notification lost")
	}
	_, err = p.Acquire("sensenova", "m", nil)
	if !errors.As(err, &selection) || !selection.OnlyReason("rate_limit_recovery") {
		t.Fatal("recovery requests were not paced", err)
	}
	next, ok = selection.WaitHint()
	if !ok || time.Until(next) < 800*time.Millisecond {
		t.Fatal("recovery pacing deadline missing")
	}
	other, err := p.Acquire("sensenova", "other", nil)
	if err != nil {
		t.Fatal("model recovery blocked unrelated model", err)
	}
	p.Finish(other.ID, 200, Usage{}, false, 0, "other")
	time.Sleep(time.Until(next) + time.Millisecond)
	a, err = p.Acquire("sensenova", "m", nil)
	if err != nil {
		t.Fatal("paced request did not recover", err)
	}
	p.Finish(a.ID, 200, Usage{}, false, 0, "m")
}

func TestWaitHintIgnoresPermanentRejections(t *testing.T) {
	for _, reason := range []string{"model_not_supported", ReasonModelDenied, "quota_exhausted", "account_disabled", "invalid_upstream_credential", "already_attempted"} {
		e := &AcquireError{Rejections: []Rejection{{Reason: reason}}}
		if _, wait := e.WaitHint(); wait {
			t.Fatalf("permanent reason queued: %s", reason)
		}
	}
	e := &AcquireError{Rejections: []Rejection{{Reason: "account_disabled"}, {Reason: "model_cooldown", RetryAt: time.Now().Add(time.Second)}}}
	if _, wait := e.WaitHint(); !wait {
		t.Fatal("mixed pool did not wait for its recoverable account")
	}
}
