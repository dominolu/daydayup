package provider

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"safuno/internal/config"
)

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
	if err := r.Register("bad name", SenseNova{}); err == nil {
		t.Fatal("names with spaces must fail")
	}
}

func TestRegistryDescriptorAttachesInfo(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("sensenova", SenseNova{}); err != nil {
		t.Fatal(err)
	}
	info, ok := r.Info("sensenova")
	if !ok || info.ID != "sensenova" || info.Name != "SenseNova" || info.BaseURL == "" || info.Auth != "api_key" {
		t.Fatalf("descriptor not attached: %+v %v", info, ok)
	}
	if _, ok := r.Info("unknown"); ok {
		t.Fatal("unknown platform must have no description")
	}
}

func TestRegistryRemoveAndReregisterRestoresDescription(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("generic1", Generic{}); err != nil {
		t.Fatal(err)
	}
	r.SetInfo(Info{ID: "generic1", Name: "热注册平台", Hot: true})
	if !r.Remove("generic1") {
		t.Fatal("remove must report success")
	}
	if r.Has("generic1") {
		t.Fatal("removed provider still registered")
	}
	if _, ok := r.Info("generic1"); ok {
		t.Fatal("removed provider still described")
	}
	if r.Remove("generic1") {
		t.Fatal("removing a missing provider must report false")
	}
	// Re-registering the same name restores the operator's description rather
	// than letting Generic's empty descriptor claim the slot.
	if err := r.Register("generic1", Generic{}); err != nil {
		t.Fatal(err)
	}
	info, _ := r.Info("generic1")
	if info.Name != "热注册平台" || !info.Hot {
		t.Fatalf("description not restored: %+v", info)
	}
	if names := r.Names(); len(names) != 1 || names[0] != "generic1" {
		t.Fatalf("re-registered name listed twice: %v", names)
	}
}

func TestRegistrySetInfoBeforeRegisterIsHeld(t *testing.T) {
	r := NewRegistry()
	r.SetInfo(Info{ID: "later", Name: "Later", Hot: true})
	if _, ok := r.Info("later"); ok {
		t.Fatal("description of an unregistered name must not be listed")
	}
	if err := r.Register("later", Generic{}); err != nil {
		t.Fatal(err)
	}
	if info, ok := r.Info("later"); !ok || info.Name != "Later" {
		t.Fatalf("held description lost: %+v %v", info, ok)
	}
}

func TestRegistryChangeHooksFireAfterMutation(t *testing.T) {
	r := NewRegistry()
	var mu sync.Mutex
	seen := []int{}
	r.OnChange(func() {
		mu.Lock()
		seen = append(seen, len(r.Names()))
		mu.Unlock()
	})
	if err := r.Register("a", Generic{}); err != nil {
		t.Fatal(err)
	}
	_ = r.Swap("a", Generic{})
	_ = r.Replace("b", Generic{})
	r.Remove("a")
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(seen) != "[1 1 2 1]" {
		t.Fatalf("hook observations: %v", seen)
	}
}

func TestRegistryConcurrentReadersAndMutators(t *testing.T) {
	r := NewRegistry()
	_ = r.Register("sensenova", SenseNova{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("p%d", i)
			for j := 0; j < 50; j++ {
				_ = r.Register(name, Generic{})
				r.SetInfo(Info{ID: name, Name: name, Hot: true})
				_, _ = r.Info(name)
				_, _ = r.Get(name)
				_ = r.Names()
				r.Remove(name)
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				for _, p := range r.Snapshot() {
					_ = p.Name()
				}
			}
		}()
	}
	wg.Wait()
	names := r.Names()
	if len(names) != 1 || names[0] != "sensenova" {
		t.Fatalf("after concurrent churn: %v", names)
	}
}

// stubProvider is a minimal adapter for registry tests.
type stubProvider struct{}

func (stubProvider) Name() string                            { return "stub" }
func (stubProvider) Authorize(*http.Request, config.Account) {}
func (stubProvider) Models(context.Context, *http.Client, config.Account) ([]Model, error) {
	return nil, nil
}
func (stubProvider) Quota(context.Context, *http.Client, config.Account) (Quota, error) {
	return Quota{}, nil
}
func (stubProvider) ModelRateLimit([]byte) bool   { return false }
func (stubProvider) ModelUnavailable([]byte) bool { return false }
