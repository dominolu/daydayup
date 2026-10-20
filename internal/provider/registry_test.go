package provider

import (
	"context"
	"net/http"
	"testing"

	"daydayup/internal/config"
)

func TestRegistryRegisterGetNames(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("sensenova", SenseNova{}); err != nil {
		t.Fatalf("register sensenova: %v", err)
	}
	if err := r.Register("workbuddy", stubProvider{}); err != nil {
		t.Fatalf("register workbuddy: %v", err)
	}
	if _, ok := r.Get("sensenova"); !ok {
		t.Fatal("sensenova should be registered")
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("missing provider must not resolve")
	}
	names := r.Names()
	if len(names) != 2 || names[0] != "sensenova" || names[1] != "workbuddy" {
		t.Fatalf("unexpected names: %v", names)
	}
}

func TestRegistryRejectsDuplicateAndEmpty(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("sensenova", SenseNova{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register("sensenova", SenseNova{}); err == nil {
		t.Fatal("duplicate registration must fail")
	}
	if err := r.Register("", SenseNova{}); err == nil {
		t.Fatal("empty provider name must fail")
	}
}

type stubProvider struct{}

func (stubProvider) Name() string                            { return "stub" }
func (stubProvider) Authorize(*http.Request, config.Account) {}
func (stubProvider) Models(context.Context, *http.Client, config.Account) ([]Model, error) {
	return nil, nil
}
func (stubProvider) Quota(context.Context, *http.Client, config.Account) (Quota, error) {
	return Quota{}, nil
}
func (stubProvider) ModelRateLimit([]byte) bool { return false }
func (stubProvider) ModelUnavailable([]byte) bool {
	return false
}
