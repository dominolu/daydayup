package gateway

// M4 daily pre-warm (P2-4): before real traffic arrives, issue one tiny chat
// turn per eligible account so its quota window (e.g. the WorkBuddy credit
// 5h window) is already open at reset time. The request goes straight to the
// provider with the account's own session-derived headers; it never reserves
// pool capacity and never touches pool accounting.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"daydayup/internal/pool"
	"daydayup/internal/provider"
)

// prewarmHour returns the configured hour with the WorkBuddy reset default.
func (s *Server) prewarmHour() int {
	if s.Config.Prewarm.Hour == nil {
		return 4
	}
	return *s.Config.Prewarm.Hour
}

// nextPrewarmTime returns the next local wall clock at the prewarm hour.
func nextPrewarmTime(now time.Time, hour int) time.Time {
	cand := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if cand.After(now) {
		return cand
	}
	return cand.AddDate(0, 0, 1)
}

// prewarmLoop fires the scan once per day at the configured hour.
func (s *Server) prewarmLoop(ctx context.Context) {
	if !s.Config.Prewarm.Enabled {
		return
	}
	hour := s.prewarmHour()
	for {
		next := nextPrewarmTime(time.Now(), hour)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.runPrewarm(ctx, "scheduler")
	}
}

// runPrewarm pings every eligible account once. source is "scheduler" or the
// admin trigger label.
func (s *Server) runPrewarm(ctx context.Context, source string) {
	if !s.Config.Prewarm.Enabled && source != "manual" {
		return
	}
	started := time.Now()
	okCount, failCount, skipCount := 0, 0, 0
	for _, st := range s.Pool.Snapshot() {
		if ctx.Err() != nil {
			return
		}
		if !taskEligible(st) {
			skipCount++
			continue
		}
		model := firstModel(st)
		if model == "" {
			skipCount++
			continue
		}
		if err := s.prewarmAccount(ctx, st, model); err != nil {
			failCount++
			slog.Debug("prewarm account failed", "account", st.ID, "error", err)
			continue
		}
		okCount++
	}
	slog.Info("prewarm finished", "source", source, "ok", okCount, "failed", failCount, "skipped", skipCount, "duration_ms", time.Since(started).Milliseconds())
}

// firstModel picks the first discovered model for the ping, or the first
// allowlisted model; discovery order is the upstream's own. The ping costs
// max_tokens=1, so the choice is cost-negligible.
func firstModel(st pool.State) string {
	for _, m := range st.Models {
		if id, ok := m["id"].(string); ok && id != "" {
			return id
		}
	}
	if len(st.ModelsAllowlist) > 0 {
		return st.ModelsAllowlist[0]
	}
	return ""
}

// prewarmAccount sends one max_tokens=1 chat turn straight to the account's
// upstream with the provider's own identity and deterministic session headers.
// No pool slot is reserved and no accounting is mutated.
func (s *Server) prewarmAccount(ctx context.Context, st pool.State, model string) error {
	a, err := s.Pool.Account(st.ID)
	if err != nil {
		return err
	}
	p, ok := s.providers.Get(a.Provider)
	if !ok {
		return nil
	}
	timeout := s.Config.Prewarm.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	preCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	body, _ := json.Marshal(map[string]any{
		"model":     model,
		"messages":  []map[string]string{{"role": "user", "content": "prewarm"}},
		"max_tokens": 1,
		"stream":    false,
	})
	if tr, ok := p.(provider.BodyTransformer); ok {
		body, err = tr.TransformRequestBody(http.MethodPost, "/v1/chat/completions", "application/json", body, a)
		if err != nil {
			return err
		}
	}
	path := "/v1/chat/completions"
	if rw, ok := p.(provider.PathRewriter); ok {
		if rewritten, ok := rw.RewritePath(path); ok {
			path = rewritten
		}
	}
	target, err := url.Parse(a.BaseURL)
	if err != nil {
		return err
	}
	target.Path = path
	req, err := http.NewRequestWithContext(preCtx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// A deterministic per-account session keeps the conversation headers
	// stable so the upstream accepts the probe without business state.
	if sa, ok := p.(provider.SessionAuthorizer); ok {
		sa.AuthorizeSession(req, a, "wb-prewarm-"+st.ID, model)
	} else {
		p.Authorize(req, a)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused by the pool.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		slog.Info("prewarm ok", "account", st.ID, "model", model, "status", resp.StatusCode)
		return nil
	}
	return fmt.Errorf("prewarm upstream HTTP %d", resp.StatusCode)
}
