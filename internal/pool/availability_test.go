package pool

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPermissionRejectionIsOnlyDefinitiveWhenAllAccountsDenyModel(t *testing.T) {
	p := New(accounts())
	p.DenyModel("a", "denied", time.Minute)
	for i := 0; i < 2; i++ {
		if _, err := p.Acquire("sensenova", "allowed", map[string]bool{"a": true}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := p.Acquire("sensenova", "denied", nil)
	var selection *AcquireError
	if !errors.As(err, &selection) || selection.OnlyReason(ReasonModelDenied) || len(selection.Rejections) != 2 {
		t.Fatal("a busy account with model access must not be classified as a permission denial")
	}
	if !strings.Contains(err.Error(), "concurrency_limit=1") || !strings.Contains(err.Error(), "model_not_in_token_plan=1") {
		t.Fatal("mixed scheduling reasons were not explained")
	}
	for i := 0; i < 2; i++ {
		p.Finish("b", 200, Usage{}, false, 0, "allowed")
	}
	a, err := p.Acquire("sensenova", "denied", nil)
	if err != nil || a.ID != "b" {
		t.Fatal("a model denial on one account blocked another eligible account")
	}
	p.Finish(a.ID, 200, Usage{}, false, 0, "denied")
	p.DenyModel("b", "denied", time.Minute)
	_, err = p.Acquire("sensenova", "denied", nil)
	if !errors.As(err, &selection) || !selection.OnlyReason(ReasonModelDenied) {
		t.Fatal("all-account model permission denial was not identified")
	}
	// A denial for this model must not affect another model.
	a, err = p.Acquire("sensenova", "allowed", nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Finish(a.ID, 200, Usage{}, false, 0, "allowed")
	p.mu.Lock()
	p.entries[0].deniedModels["denied"] = time.Now().Add(-time.Second)
	p.mu.Unlock()
	a, err = p.Acquire("sensenova", "denied", nil)
	if err != nil || a.ID != "a" {
		t.Fatal("expired denial did not recover")
	}
	p.Finish(a.ID, 200, Usage{}, false, 0, "denied")
}

func TestEmptyPoolDoesNotClaimModelPermissionDenied(t *testing.T) {
	p := New(nil)
	_, err := p.Acquire("sensenova", "m", nil)
	var selection *AcquireError
	if !errors.As(err, &selection) || selection.OnlyReason(ReasonModelDenied) || !strings.Contains(err.Error(), "no accounts configured") {
		t.Fatal("empty pool was not explained correctly")
	}
}
