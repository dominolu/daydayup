package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestAdminPerGroupRoutingStrategy(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}), 1)

	// Pin the default group to smart.
	w := adminCall(s, http.MethodPut, "/admin/v1/groups/default/routing", `{"strategy":"smart"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put routing: %d %s", w.Code, w.Body.String())
	}
	var r0 map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &r0); err != nil {
		t.Fatal(err)
	}
	if r0["routing"] != "smart" {
		t.Fatalf("response routing=%v want smart", r0["routing"])
	}
	if got := s.Pool.RoutingFor("default"); got != "smart" {
		t.Fatalf("pool default routing=%s want smart", got)
	}

	// GET /admin/v1/groups reflects the effective per-group strategy.
	w = adminCall(s, http.MethodGet, "/admin/v1/groups", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get groups: %d", w.Code)
	}
	var groups struct {
		Data []struct {
			Name            string `json:"name"`
			Routing         string `json:"routing"`
			RoutingOverride string `json:"routing_override"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range groups.Data {
		if g.Name == "default" {
			found = true
			if g.Routing != "smart" || g.RoutingOverride != "smart" {
				t.Fatalf("default group routing info: %+v", g)
			}
		}
	}
	if !found {
		t.Fatal("default group missing from groups list")
	}

	// Invalid strategy is rejected.
	w = adminCall(s, http.MethodPut, "/admin/v1/groups/default/routing", `{"strategy":"weird"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid strategy: %d", w.Code)
	}

	// Overrides are isolated per group; a new group does not disturb default.
	w = adminCall(s, http.MethodPut, "/admin/v1/groups/worker-a/routing", `{"strategy":"round_robin"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put worker-a: %d %s", w.Code, w.Body.String())
	}
	if got := s.Pool.RoutingFor("worker-a"); got != "round_robin" {
		t.Fatalf("worker-a routing=%s want round_robin", got)
	}
	if got := s.Pool.RoutingFor("default"); got != "smart" {
		t.Fatalf("default routing changed to %s", got)
	}
	if got := s.Pool.RoutingFor("untouched"); got != s.Pool.Routing() {
		t.Fatalf("untouched group should follow global routing, got %s", got)
	}
}
