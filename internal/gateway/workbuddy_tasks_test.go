package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/pool"
	"daydayup/internal/provider/workbuddy"
)

func newTaskGateway(t *testing.T, dir string, accounts []config.Account) *Server {
	t.Helper()
	c := config.Config{DefaultProvider: "workbuddy", GatewayKey: "gateway-secret-at-least-16", AdminKey: "admin-secret-at-least-16", RequestTimeoutSeconds: 5, RefreshIntervalSeconds: 60, MaxRequestBytes: 1 << 20, MaxAttempts: 3, DBFile: dir + "/gateway.db", Accounts: accounts}
	s := New(c)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return s
}

// tasksUpstream serves the check-in / report / streak / chat endpoints that
// the bonus tasks hit, keyed by realm.
func tasksUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin", "/v2/billing/meter/checkin-activity-status":
			_, _ = io.WriteString(w, `{"code":0,"data":{"today_checked_in":true,"streak_days":4,"daily_credit":3,"today_credit":3,"active":true}}`)
		case "/v2/report":
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK"}`)
		case "/activity/growth/streak":
			_, _ = io.WriteString(w, `{"code":0,"data":{"streakDays":7}}`)
		case "/v2/chat/completions":
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestNextWorkBuddyTaskTime(t *testing.T) {
	zone := time.FixedZone("CST", 8*3600)
	cases := []struct{ now, want string }{
		{"2026-10-19 08:00:00", "2026-10-19 09:15:00"},
		{"2026-10-19 10:00:00", "2026-10-19 21:15:00"},
		{"2026-10-19 22:00:00", "2026-10-20 09:15:00"},
		{"2026-10-19 09:15:00", "2026-10-19 21:15:00"}, // exactly at a window -> next one
	}
	for _, tc := range cases {
		now, _ := time.ParseInLocation("2006-01-02 15:04:05", tc.now, zone)
		want, _ := time.ParseInLocation("2006-01-02 15:04:05", tc.want, zone)
		if got := nextWorkBuddyTaskTime(now); !got.Equal(want) {
			t.Errorf("%s -> %v, want %v", tc.now, got, want)
		}
	}
}

func TestWorkBuddyTaskEligibility(t *testing.T) {
	base := pool.State{Realm: "cn", Enabled: true, CredentialStatus: "valid"}
	if !canWorkBuddyCheckin(base) {
		t.Fatal("fresh cn account must be due for check-in")
	}
	done := base
	done.CheckinDate = time.Now().Format("2006-01-02")
	if canWorkBuddyCheckin(done) {
		t.Fatal("today's check-in must not be due again")
	}
	if canWorkBuddyDailyChat(base) {
		t.Fatal("daily chat is intl-only")
	}
	intl := pool.State{Realm: "intl", Enabled: true, CredentialStatus: "valid"}
	if !canWorkBuddyDailyChat(intl) {
		t.Fatal("fresh intl account must be due for daily chat")
	}
	invalid := base
	invalid.Enabled = false
	if taskEligible(invalid) {
		t.Fatal("disabled account must be skipped")
	}
}

func TestWorkBuddyTaskAdminCheckin(t *testing.T) {
	up := tasksUpstream(t)
	restore := workbuddy.OverrideUpstream("cn", up.URL)
	defer restore()
	dir := t.TempDir()
	s := newTaskGateway(t, dir, []config.Account{{ID: "wb-cn", Provider: "workbuddy", APIKey: "at", RefreshToken: "rt", UID: "u-cn", Realm: "cn", Product: "desktop", MaxInFlight: 4}})

	w := oauthAdminCall(s, "/accounts/wb-cn/tasks/checkin", "")
	if w.Code != 200 {
		t.Fatalf("checkin: %d %s", w.Code, w.Body)
	}
	var resp struct {
		Ok      bool   `json:"ok"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Ok {
		t.Fatalf("resp: %s", w.Body)
	}
	state := s.accountStates("wb-cn").(pool.State)
	if state.CheckinDate != time.Now().Format("2006-01-02") {
		t.Fatalf("checkin date not stamped: %+v", state.CheckinDate)
	}
	if state.CheckinInfo == nil || state.CheckinInfo.StreakDays != 4 {
		t.Fatalf("checkin info not refreshed: %+v", state.CheckinInfo)
	}
	// Registered store file must carry the new fields after checkpoint.
	if err := s.Pool.Save(s.Config.StateFile); err != nil {
		t.Fatal(err)
	}
}

func TestWorkBuddyTaskAdminWrongRealm(t *testing.T) {
	up := tasksUpstream(t)
	restore := workbuddy.OverrideUpstream("cn", up.URL)
	defer restore()
	s := newTaskGateway(t, t.TempDir(), []config.Account{{ID: "wb-intl", Provider: "workbuddy", APIKey: "at", RefreshToken: "rt", UID: "u-intl", Realm: "intl", Product: "desktop", MaxInFlight: 4}})
	w := oauthAdminCall(s, "/accounts/wb-intl/tasks/checkin", "")
	if w.Code != 400 {
		t.Fatalf("cn-only task on intl account: %d %s", w.Code, w.Body)
	}
	w2 := oauthAdminCall(s, "/accounts/wb-intl/tasks/unknown", "")
	if w2.Code != 400 {
		t.Fatalf("unknown task kind: %d %s", w2.Code, w2.Body)
	}
}

func TestWorkBuddyTasksScan(t *testing.T) {
	up := tasksUpstream(t)
	r1 := workbuddy.OverrideUpstream("cn", up.URL)
	r2 := workbuddy.OverrideUpstream("intl", up.URL)
	defer r1()
	defer r2()
	s := newTaskGateway(t, t.TempDir(), []config.Account{
		{ID: "wb-cn", Provider: "workbuddy", APIKey: "at", RefreshToken: "rt", UID: "u-cn", Realm: "cn", Product: "desktop", MaxInFlight: 4},
		{ID: "wb-intl", Provider: "workbuddy", APIKey: "at2", RefreshToken: "rt2", UID: "u-intl", Realm: "intl", Product: "desktop", MaxInFlight: 4},
	})
	s.runWorkBuddyTasks(context.Background(), "test")
	cn := s.accountStates("wb-cn").(pool.State)
	in := s.accountStates("wb-intl").(pool.State)
	today := time.Now().Format("2006-01-02")
	if cn.CheckinDate != today || cn.ActivityReportDate != today {
		t.Fatalf("cn claim dates: checkin=%q activity=%q", cn.CheckinDate, cn.ActivityReportDate)
	}
	if in.DailyChatDate != today {
		t.Fatalf("intl claim date: %q", in.DailyChatDate)
	}
	if cn.CheckinInfo == nil || cn.CheckinInfo.StreakDays != 4 {
		t.Fatalf("checkin info: %+v", cn.CheckinInfo)
	}
	// A second scan the same day must not double-claim: upstream call counts
	// stay at exactly one per account.
	var calls struct {
		Checkin int
		Report  int
		Chat    int
	}
	up.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			calls.Checkin++
			_, _ = io.WriteString(w, `{"code":0}`)
		case "/v2/billing/meter/checkin-activity-status":
			_, _ = io.WriteString(w, `{"code":0,"data":{"today_checked_in":true,"streak_days":5,"daily_credit":3,"today_credit":3,"active":true}}`)
		case "/v2/report":
			calls.Report++
			_, _ = io.WriteString(w, `{"code":0}`)
		case "/v2/chat/completions":
			calls.Chat++
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
	})
	s.runWorkBuddyTasks(context.Background(), "test")
	if calls.Checkin != 0 || calls.Report != 0 || calls.Chat != 0 {
		t.Fatalf("second scan must be a no-op: %+v", calls)
	}
}

func TestWorkBuddyTaskAdminWrongKindForRealm(t *testing.T) {
	up := tasksUpstream(t)
	restore := workbuddy.OverrideUpstream("cn", up.URL)
	defer restore()
	s := newTaskGateway(t, t.TempDir(), []config.Account{{ID: "wb-cn", Provider: "workbuddy", APIKey: "at", RefreshToken: "rt", UID: "u-cn", Realm: "cn", Product: "desktop", MaxInFlight: 4}})
	w := oauthAdminCall(s, "/accounts/wb-cn/tasks/daily-chat", "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "国际版") {
		t.Fatalf("want realm error: %d %s", w.Code, w.Body)
	}
}
