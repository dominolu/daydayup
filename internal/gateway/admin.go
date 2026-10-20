package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"daydayup/internal/pool"
	"daydayup/internal/provider"
)

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/v1/client-key" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"api_key": s.Config.GatewayKey})
		return
	}
	if s.agentAdmin(w, r) {
		return
	}
	if s.sessionAdmin(w, r) {
		return
	}
	if s.accountAdmin(w, r) {
		return
	}
	path := r.URL.Path
	if path == "/admin/v1/accounts" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"data": s.Pool.Snapshot()})
		return
	}
	if path == "/admin/v1/platforms" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"data": platformCatalog()})
		return
	}
	if path == "/admin/v1/groups" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"data": s.Pool.Groups(), "default_group": pool.DefaultGroup, "config_default_group": s.Config.DefaultGroup})
		return
	}
	if path == "/admin/v1/providers" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"data": s.providers.Names(), "quota_sync": "external_snapshot", "official_quota_api": "unsupported"})
		return
	}
	if path == "/admin/v1/redaction" && r.Method == http.MethodGet {
		s.redactionAdmin(w, r)
		return
	}
	if path == "/admin/v1/redaction/rules" && r.Method == http.MethodPut {
		s.redactionUpdateRules(w, r)
		return
	}
	if path == "/admin/v1/redaction/spans" && r.Method == http.MethodGet {
		s.redactionSpansAdmin(w, r)
		return
	}
	if path == "/admin/v1/usage" || path == "/admin/v1/routing" {
		s.usageAdmin(w, r)
		return
	}
	if path == "/admin/v1/refresh" && r.Method == http.MethodPost {
		s.Refresh(r.Context())
		writeJSON(w, 200, map[string]any{"data": s.Pool.Snapshot()})
		return
	}
	if path == "/metrics" && r.Method == http.MethodGet {
		s.metrics(w)
		return
	}
	if strings.HasPrefix(path, "/admin/v1/accounts/") {
		parts := strings.Split(strings.TrimPrefix(path, "/admin/v1/accounts/"), "/")
		if len(parts) == 2 {
			var err error
			switch {
			case parts[1] == "quota" && r.Method == http.MethodPut:
				var q provider.Quota
				if !decodeJSON(w, r, &q) {
					return
				}
				err = s.Pool.SetQuota(parts[0], &q)
			case parts[1] == "quota" && r.Method == http.MethodDelete:
				err = s.Pool.SetQuota(parts[0], nil)
			case parts[1] == "enabled" && r.Method == http.MethodPut:
				var body struct {
					Enabled *bool `json:"enabled"`
				}
				if !decodeJSON(w, r, &body) {
					return
				}
				if body.Enabled == nil {
					apiError(w, 400, "invalid_request", "enabled is required")
					return
				}
				err = s.Pool.SetEnabled(parts[0], *body.Enabled)
			default:
				apiError(w, 404, "not_found", "admin endpoint not found")
				return
			}
			if errors.Is(err, os.ErrNotExist) {
				apiError(w, 404, "account_not_found", "account not found")
				return
			}
			if err != nil {
				apiError(w, 400, "invalid_request", err.Error())
				return
			}
			if err = s.Pool.Save(s.Config.DBFile); err != nil {
				apiError(w, 500, "persistence_failed", "change applied in memory, but checkpoint failed")
				return
			}
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
	}
	apiError(w, 404, "not_found", "admin endpoint not found")
}
func (s *Server) metrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, a := range s.Pool.Snapshot() {
		labels := fmt.Sprintf("account=%q,provider=%q", a.ID, a.Provider)
		fmt.Fprintf(w, "gateway_requests_total{%s} %d\ngateway_failures_total{%s} %d\ngateway_tokens_total{%s} %d\ngateway_usage_unknown_total{%s} %d\ngateway_in_flight{%s} %d\n", labels, a.Stats.Requests, labels, a.Stats.Failures, labels, a.Stats.Total, labels, a.Stats.UnknownUsage, labels, a.InFlight)
		fresh := pool.QuotaFresh(a.Quota, time.Now())
		if fresh && a.Quota.RemainingTokens != nil {
			fmt.Fprintf(w, "gateway_remaining_tokens{%s} %d\n", labels, *a.Quota.RemainingTokens)
		}
		if fresh {
			for _, b := range a.Quota.Buckets {
				fmt.Fprintf(w, "gateway_quota_remaining{%s,bucket=%q,pool=%q,unit=%q} %g\n", labels, b.Name, b.Pool, b.Unit, b.Remaining)
			}
		}
		status := 0
		if fresh {
			status = 1
		}
		fmt.Fprintf(w, "gateway_quota_fresh{%s} %d\n", labels, status)
	}
	if s.redact != nil {
		st := s.redact.Stats()
		fmt.Fprintf(w, "gateway_redaction_masked_total %d\ngateway_redaction_restored_total %d\ngateway_redaction_unresolved_total %d\ngateway_redaction_mappings %d\n", st.Masked, st.Restored, st.Unresolved, st.Mappings)
	}
}
