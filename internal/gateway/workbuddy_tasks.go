package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"daydayup/internal/pool"
	"daydayup/internal/provider"
)

// WorkBuddy daily-bonus tasks, mirroring the workbuddy2api-hub scheduler:
// CN daily check-in (credits), CN active report (growth streak) and the intl
// daily activity chat. The scheduler runs them twice a day (09:15 / 21:15
// local); the same code backs the manual admin endpoints. Each account is
// stamped with a wall-clock date ("2006-01-02") when its claim succeeds, so a
// restart or a duplicate trigger never double-claims the same day.

// workBuddyTasks returns the registered workbuddy provider as its DailyTasks
// surface, or nil when the provider is absent.
func (s *Server) workBuddyTasks() provider.DailyTasks {
	p, ok := s.providers.Get("workbuddy")
	if !ok {
		return nil
	}
	if dt, ok := p.(provider.DailyTasks); ok {
		return dt
	}
	return nil
}

func todayStr() string { return time.Now().Format("2006-01-02") }

func canWorkBuddyCheckin(st pool.State) bool {
	return st.Realm == "cn" && st.CheckinDate != todayStr()
}

func canWorkBuddyActivityReport(st pool.State) bool {
	return st.Realm == "cn" && st.ActivityReportDate != todayStr()
}

func canWorkBuddyDailyChat(st pool.State) bool {
	return st.Realm == "intl" && st.DailyChatDate != todayStr()
}

// taskEligible skips accounts that must not be touched by unattended bonus
// claims: disabled, credential-invalid, cooled, quota-parked or region-wide
// restricted accounts (restrictions are shared across the realm).
func taskEligible(st pool.State) bool {
	if !st.Enabled || st.CredentialStatus == "invalid" {
		return false
	}
	now := time.Now()
	if !st.CooldownUntil.IsZero() && now.Before(st.CooldownUntil) {
		return false
	}
	if st.QuotaExhaustedUntil != nil && now.Before(*st.QuotaExhaustedUntil) {
		return false
	}
	return true
}

// nextWorkBuddyTaskTime returns the next 09:15 / 21:15 local window.
func nextWorkBuddyTaskTime(now time.Time) time.Time {
	for _, hh := range []int{9, 21} {
		cand := time.Date(now.Year(), now.Month(), now.Day(), hh, 15, 0, 0, now.Location())
		if cand.After(now) {
			return cand
		}
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 9, 15, 0, 0, now.Location())
}

// workBuddyTasksLoop fires the daily bonus scan at each window until ctx is
// done. Claims are per-day guarded inside the scan, so late wake-ups are safe.
func (s *Server) workBuddyTasksLoop(ctx context.Context) {
	for {
		next := nextWorkBuddyTaskTime(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.runWorkBuddyTasks(ctx, "scheduler")
	}
}

// runWorkBuddyTasks walks every WorkBuddy account and runs whatever bonus task
// is due today (CN check-in + active report, intl daily chat).
func (s *Server) runWorkBuddyTasks(ctx context.Context, trigger string) {
	dt := s.workBuddyTasks()
	if dt == nil {
		return
	}
	states := map[string]pool.State{}
	for _, st := range s.Pool.Snapshot() {
		states[st.ID] = st
	}
	accounts := s.Pool.Accounts()
	if len(accounts) == 0 {
		slog.Info("workbuddy bonus scan: no accounts")
		return
	}
	checkin, report, chat := 0, 0, 0
	for idx, a := range accounts {
		if a.Provider != "workbuddy" || ctx.Err() != nil {
			continue
		}
		st, ok := states[a.ID]
		if !ok || !taskEligible(st) {
			continue
		}
		if idx > 0 {
			// Per-account stagger keeps the daily burst off one device
			// fingerprint at once (respects the ctx deadline).
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		switch st.Realm {
		case "cn":
			if canWorkBuddyCheckin(st) {
				if _, err := dt.Checkin(ctx, s.client, a); err == nil {
					_ = s.Pool.SetWorkBuddyTask(a.ID, "checkin", todayStr(), nil)
					checkin++
					slog.Info("workbuddy checkin ok", "account", st.ID)
				} else {
					slog.Warn("workbuddy checkin failed", "account", st.ID, "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
			if canWorkBuddyActivityReport(st) {
				if res, err := dt.ActivityReport(ctx, s.client, a); err == nil {
					_ = s.Pool.SetWorkBuddyTask(a.ID, "activity", todayStr(), nil)
					report++
					slog.Info("workbuddy activity report ok", "account", st.ID, "message", res.Message)
				} else {
					slog.Warn("workbuddy activity report failed", "account", st.ID, "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
			// Refresh the CN check-in snapshot for the admin UI (best effort;
			// one status read per CN account per scan is cheap).
			if info, err := dt.CheckinStatus(ctx, s.client, a); err == nil {
				_ = s.Pool.SetWorkBuddyTask(a.ID, "checkin", "", info)
			}
		case "intl":
			if canWorkBuddyDailyChat(st) {
				if res, err := dt.DailyChat(ctx, s.client, a); err == nil {
					_ = s.Pool.SetWorkBuddyTask(a.ID, "daily_chat", todayStr(), nil)
					chat++
					slog.Info("workbuddy daily chat ok", "account", st.ID, "message", res.Message)
				} else {
					slog.Warn("workbuddy daily chat failed", "account", st.ID, "error", err)
				}
			}
		}
	}
	slog.Info("workbuddy bonus scan done", "trigger", trigger, "checkin", checkin, "activity_report", report, "daily_chat", chat)
}

// workbuddyTaskAdmin runs a single bonus task for one account (manual trigger
// from the admin UI). Routes: /admin/v1/accounts/{id}/tasks/{checkin|activity|daily-chat}.
func (s *Server) workbuddyTaskAdmin(w http.ResponseWriter, id, kind string) {
	dt := s.workBuddyTasks()
	if dt == nil {
		apiError(w, 500, "provider_unavailable", "workbuddy provider is not registered")
		return
	}
	if kind != "checkin" && kind != "activity" && kind != "daily-chat" {
		apiError(w, 400, "invalid_request", "task kind must be checkin, activity or daily-chat")
		return
	}
	a, err := s.Pool.Account(id)
	if err != nil {
		mutationError(w, err)
		return
	}
	if a.Provider != "workbuddy" {
		apiError(w, 400, "invalid_account", "该任务仅适用于 WorkBuddy 账号")
		return
	}
	stAny := s.accountStates(id)
	st, ok := stAny.(pool.State)
	if !ok {
		apiError(w, 409, "account_busy", "账号状态暂不可用，请稍后重试")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var message string
	switch kind {
	case "checkin":
		if st.Realm != "cn" {
			apiError(w, 400, "invalid_realm", "每日签到仅支持国内版（cn）账号")
			return
		}
		if !canWorkBuddyCheckin(st) {
			writeJSON(w, 200, map[string]any{"ok": true, "already_done": true, "message": "今日已签到", "data": s.accountStates(id)})
			return
		}
		res, err := dt.Checkin(ctx, s.client, a)
		if err != nil {
			apiError(w, 502, "task_failed", "签到失败："+err.Error())
			return
		}
		_ = s.Pool.SetWorkBuddyTask(a.ID, "checkin", todayStr(), nil)
		if info, err := dt.CheckinStatus(ctx, s.client, a); err == nil {
			_ = s.Pool.SetWorkBuddyTask(a.ID, "checkin", "", info)
		}
		if res.AlreadyDone {
			message = "今日已签到"
		} else if res.Message != "" {
			message = "签到成功：" + res.Message
		} else {
			message = "签到成功"
		}
	case "activity":
		if st.Realm != "cn" {
			apiError(w, 400, "invalid_realm", "活跃上报仅支持国内版（cn）账号")
			return
		}
		if !canWorkBuddyActivityReport(st) {
			writeJSON(w, 200, map[string]any{"ok": true, "already_done": true, "message": "今日已活跃上报", "data": s.accountStates(id)})
			return
		}
		res, err := dt.ActivityReport(ctx, s.client, a)
		if err != nil {
			apiError(w, 502, "task_failed", "活跃上报失败："+err.Error())
			return
		}
		_ = s.Pool.SetWorkBuddyTask(a.ID, "activity", todayStr(), nil)
		message = res.Message
		if message == "" {
			message = "对话活跃上报成功"
		}
	case "daily-chat":
		if st.Realm != "intl" {
			apiError(w, 400, "invalid_realm", "每日活跃打卡仅支持国际版（intl）账号")
			return
		}
		if !canWorkBuddyDailyChat(st) {
			writeJSON(w, 200, map[string]any{"ok": true, "already_done": true, "message": "今日已打卡", "data": s.accountStates(id)})
			return
		}
		res, err := dt.DailyChat(ctx, s.client, a)
		if err != nil {
			apiError(w, 502, "task_failed", "每日活跃打卡失败："+err.Error())
			return
		}
		_ = s.Pool.SetWorkBuddyTask(a.ID, "daily_chat", todayStr(), nil)
		message = res.Message
	}
	_ = s.Pool.Save(s.Config.DBFile)
	writeJSON(w, 200, map[string]any{"ok": true, "message": message, "data": s.accountStates(id)})
}

// adminWorkBuddyTaskRoute matches /admin/v1/accounts/{id}/tasks/{kind}.
func (s *Server) adminWorkBuddyTaskRoute(w http.ResponseWriter, r *http.Request) (bool, string) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/v1/accounts/")
	parts := strings.Split(path, "/")
	if len(parts) != 3 || parts[1] != "tasks" || r.Method != http.MethodPost {
		return false, ""
	}
	return true, parts[2]
}