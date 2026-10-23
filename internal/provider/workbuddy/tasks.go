package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"safuno/internal/config"
	"safuno/internal/provider"
)

// WorkBuddy / CodeBuddy daily bonus claiming, mirroring the workbuddy2api-hub
// tasks: CN daily check-in (credit claim), CN active report (growth streak
// replay of the chat_request_send event) and the intl daily activity chat.
// The gateway schedules these once per day per account and exposes manual
// triggers; the provider implements the upstream protocol only.

const (
	checkinPath         = "/v2/billing/meter/daily-checkin"
	checkinStatusPath   = "/v2/billing/meter/checkin-activity-status"
	reportPath          = "/v2/report"
	streakPath          = "/activity/growth/streak"
	alreadyCheckedInStr = "已签到"
)

// dailyChatModel is the free intl model the daily activity chat runs on
// (same free id as the hub; x0.00 credits in this gateway's catalog).
const dailyChatModel = "deepseek-v4.1-flash"

// Checkin performs the CN realm daily check-in (credit claim). The upstream
// answers code 0 on success and 10001 when today's check-in is already done;
// both count as success so a retry after a restart never double-claims.
func (WorkBuddy) Checkin(ctx context.Context, client *http.Client, a config.Account) (provider.CheckinResult, error) {
	rc := realmOf(a)
	url := rc.billingUpstream + checkinPath
	b, status, err := doJSON(ctx, client, http.MethodPost, url, billingHeaders(a), []byte("{}"), 1<<20)
	if err != nil {
		return provider.CheckinResult{}, err
	}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return provider.CheckinResult{}, errors.New("workbuddy: invalid checkin response")
	}
	msg := strings.TrimSpace(payload.Msg)
	if status != 200 {
		return provider.CheckinResult{}, fmt.Errorf("workbuddy: checkin HTTP %d", status)
	}
	if payload.Code == 0 {
		return provider.CheckinResult{Message: msg}, nil
	}
	if payload.Code == 10001 || strings.Contains(msg, alreadyCheckedInStr) {
		return provider.CheckinResult{AlreadyDone: true, Message: msg}, nil
	}
	return provider.CheckinResult{}, fmt.Errorf("workbuddy: checkin code=%d msg=%s", payload.Code, msg)
}

// CheckinStatus reads the CN daily check-in activity snapshot so the admin UI
// can show today's state, the streak and the credit values.
func (WorkBuddy) CheckinStatus(ctx context.Context, client *http.Client, a config.Account) (*provider.CheckinStatus, error) {
	rc := realmOf(a)
	url := rc.chatUpstream + checkinStatusPath
	b, status, err := doJSON(ctx, client, http.MethodPost, url, billingHeaders(a), []byte("{}"), 1<<20)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("workbuddy: checkin status HTTP %d", status)
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			TodayCheckedIn bool    `json:"today_checked_in"`
			StreakDays     int     `json:"streak_days"`
			DailyCredit    float64 `json:"daily_credit"`
			TodayCredit    float64 `json:"today_credit"`
			Active         bool    `json:"active"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return nil, errors.New("workbuddy: invalid checkin status response")
	}
	if payload.Code != 0 {
		return nil, fmt.Errorf("workbuddy: checkin status code=%d", payload.Code)
	}
	return &provider.CheckinStatus{
		TodayCheckedIn: payload.Data.TodayCheckedIn,
		StreakDays:     payload.Data.StreakDays,
		DailyCredit:    payload.Data.DailyCredit,
		TodayCredit:    payload.Data.TodayCredit,
		Active:         payload.Data.Active,
	}, nil
}

// chatTaskHeaders is the full auth-bearing header family for the bonus-task
// upstream calls: identityHeaders alone carries no Authorization (that is
// injected separately by AuthorizeSession on the proxy path).
func chatTaskHeaders(a config.Account) map[string]string {
	headers := identityHeaders(a, "", "")
	headers["Authorization"] = "Bearer " + a.APIKey
	headers["X-User-Id"] = uidOf(a)
	headers["Content-Type"] = "application/json"
	return headers
}

// ActivityReport replays the desktop client's chat_request_send event for the
// CN growth streak (one per day, must carry userId). The streak query is
// best-effort: a streak read failure does not fail the report itself.
func (WorkBuddy) ActivityReport(ctx context.Context, client *http.Client, a config.Account) (provider.ActivityResult, error) {
	rc := realmOf(a)
	event := map[string]any{
		"eventCode":        "chat_request_send",
		"timestamp":        time.Now().UnixMilli(),
		"reportDelay":      0,
		"mode":             "craft",
		"conversationId":   fmt.Sprintf("ddn-task-%d", time.Now().UnixNano()),
		"requestId":        newMessageID(),
		"inputLength":      12,
		"requestModelId":   "deepseek-v4-flash",
		"requestModelName": "DeepSeek V4 Flash",
		"isPlan":           false,
		"agentName":        "default",
		"agentType":        "conversation",
		"userId":           uidOf(a),
	}
	body, err := json.Marshal([]any{event})
	if err != nil {
		return provider.ActivityResult{}, err
	}
	headers := chatTaskHeaders(a)
	b, status, err := doJSON(ctx, client, http.MethodPost, rc.chatUpstream+reportPath, headers, body, 1<<20)
	if err != nil {
		return provider.ActivityResult{}, err
	}
	if status != 200 {
		return provider.ActivityResult{}, fmt.Errorf("workbuddy: activity report HTTP %d", status)
	}
	var payload struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return provider.ActivityResult{}, errors.New("workbuddy: invalid activity report response")
	}
	if payload.Code != 0 {
		return provider.ActivityResult{}, fmt.Errorf("workbuddy: activity report rejected code=%d", payload.Code)
	}
	days, streakErr := fetchStreakDays(ctx, client, rc, a)
	if streakErr != nil {
		return provider.ActivityResult{Message: "对话活跃上报成功（连登天数未知）"}, nil
	}
	if days <= 0 {
		return provider.ActivityResult{Message: "对话活跃上报成功，但连登仍为 0 天"}, nil
	}
	return provider.ActivityResult{StreakDays: days, Message: fmt.Sprintf("对话活跃上报成功（连续打卡 %d 天）", days)}, nil
}

// DailyChat runs the intl daily activity chat: one light streaming turn on the
// free model (system first, like any real client chat). Note this is the
// desktop-identity leg; the official daily-activity credit counts the
// web-channel turn, which this gateway does not drive (see README).
func (WorkBuddy) DailyChat(ctx context.Context, client *http.Client, a config.Account) (provider.DailyChatResult, error) {
	rc := realmOf(a)
	body := []byte(`{"model":"` + dailyChatModel + `","messages":[{"role":"system","content":"You are WorkBuddy."},{"role":"user","content":"Hi"}],"stream":true,"max_tokens":10,"reasoning_effort":"none"}`)
	headers := chatTaskHeaders(a)
	url := rc.chatUpstream + chatPath
	b, status, err := doJSON(ctx, client, http.MethodPost, url, headers, body, 4<<20)
	if err != nil {
		return provider.DailyChatResult{}, err
	}
	if status != 200 {
		return provider.DailyChatResult{}, fmt.Errorf("workbuddy: daily chat HTTP %d", status)
	}
	// The upstream answers a streaming SSE body; count non-empty content
	// chunks as evidence the turn actually produced output.
	chunks := countSSEChunks(b)
	if chunks == 0 {
		return provider.DailyChatResult{Message: "每日活跃对话完成（无输出增量）"}, nil
	}
	return provider.DailyChatResult{Message: fmt.Sprintf("每日活跃对话完成（%d 段输出）", chunks)}, nil
}

func fetchStreakDays(ctx context.Context, client *http.Client, rc realmConfig, a config.Account) (int, error) {
	headers := chatTaskHeaders(a)
	b, status, err := doJSON(ctx, client, http.MethodGet, rc.chatUpstream+streakPath, headers, nil, 1<<20)
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, fmt.Errorf("HTTP %d", status)
	}
	var payload struct {
		Data struct {
			StreakDays any `json:"streakDays"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return 0, errors.New("invalid streak response")
	}
	return toInt(payload.Data.StreakDays), nil
}

// countSSEChunks counts SSE "data:" lines carrying a content delta. The daily
// chat body is cheap; this is only used as a lightweight reality check.
func countSSEChunks(b []byte) int {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) == nil {
			if len(ev.Choices) > 0 && ev.Choices[0].Delta.Content != "" {
				n++
			}
		}
	}
	return n
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}