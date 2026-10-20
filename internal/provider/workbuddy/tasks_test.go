package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daydayup/internal/config"
	"daydayup/internal/provider"
)

func cnTestAccount() config.Account {
	return testAccount("cn-at-1", "cn-rt-1", "uid-cn", "cn", "desktop")
}

func intlTestAccount() config.Account {
	return testAccount("intl-at-1", "intl-rt-1", "uid-intl", "intl", "desktop")
}

// tasksMock serves the check-in / report / daily-chat endpoints with a
// pluggable per-path responder.
func tasksMock(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(up.Close)
	return up
}

func TestCheckinSuccess(t *testing.T) {
	var sawPath, sawBody string
	up := tasksMock(t, func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		sawBody = string(b)
		_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"credit":3}}`)
	})
	restore := OverrideUpstream("cn", up.URL)
	defer restore()

	res, err := (WorkBuddy{}).Checkin(context.Background(), up.Client(), cnTestAccount())
	if err != nil {
		t.Fatal(err)
	}
	if sawPath != checkinPath || sawBody != "{}" {
		t.Fatalf("path=%q body=%q", sawPath, sawBody)
	}
	if res.AlreadyDone {
		t.Fatal("fresh check-in must not be already-done")
	}
}

func TestCheckinAlreadyDone(t *testing.T) {
	up := tasksMock(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":10001,"msg":"今天已签到"}`)
	})
	restore := OverrideUpstream("cn", up.URL)
	defer restore()

	res, err := (WorkBuddy{}).Checkin(context.Background(), up.Client(), cnTestAccount())
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyDone {
		t.Fatalf("want already-done, got %+v", res)
	}
}

func TestCheckinUpstreamFailure(t *testing.T) {
	up := tasksMock(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":40012,"msg":"not allowed"}`)
	})
	restore := OverrideUpstream("cn", up.URL)
	defer restore()

	if _, err := (WorkBuddy{}).Checkin(context.Background(), up.Client(), cnTestAccount()); err == nil || !strings.Contains(err.Error(), "40012") {
		t.Fatalf("want code error, got %v", err)
	}
}

func TestCheckinStatus(t *testing.T) {
	var path string
	up := tasksMock(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = io.WriteString(w, `{"code":0,"data":{"today_checked_in":false,"streak_days":5,"daily_credit":3,"today_credit":0,"active":true}}`)
	})
	restore := OverrideUpstream("cn", up.URL)
	defer restore()

	st, err := (WorkBuddy{}).CheckinStatus(context.Background(), up.Client(), cnTestAccount())
	if err != nil {
		t.Fatal(err)
	}
	if path != checkinStatusPath {
		t.Fatalf("path %q", path)
	}
	if st.StreakDays != 5 || st.DailyCredit != 3 || st.TodayCheckedIn || !st.Active {
		t.Fatalf("status: %+v", st)
	}
}

func TestActivityReport(t *testing.T) {
	var reported []map[string]any
	var streakCalled bool
	up := tasksMock(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case reportPath:
			_ = json.NewDecoder(r.Body).Decode(&reported)
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK"}`)
		case streakPath:
			streakCalled = true
			_, _ = io.WriteString(w, `{"code":0,"data":{"streakDays":6}}`)
		default:
			http.NotFound(w, r)
		}
	})
	restore := OverrideUpstream("cn", up.URL)
	defer restore()

	res, err := (WorkBuddy{}).ActivityReport(context.Background(), up.Client(), cnTestAccount())
	if err != nil {
		t.Fatal(err)
	}
	if len(reported) != 1 {
		t.Fatalf("events: %v", reported)
	}
	ev := reported[0]
	if ev["eventCode"] != "chat_request_send" || ev["userId"] != "uid-cn" {
		t.Fatalf("event: %v", ev)
	}
	if !streakCalled || res.StreakDays != 6 {
		t.Fatalf("streak=%v called=%v res=%+v", res.StreakDays, streakCalled, res)
	}
}

func TestDailyChat(t *testing.T) {
	var body string
	up := tasksMock(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		// A minimal streaming SSE body with one content delta.
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	})
	restore := OverrideUpstream("intl", up.URL)
	defer restore()

	res, err := (WorkBuddy{}).DailyChat(context.Background(), up.Client(), intlTestAccount())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"stream":true`) || !strings.Contains(body, `"max_tokens":10`) {
		t.Fatalf("body: %s", body)
	}
	if !strings.Contains(body, dailyChatModel) {
		t.Fatalf("model missing: %s", body)
	}
	if !strings.Contains(res.Message, "1 段输出") {
		t.Fatalf("message: %s", res.Message)
	}
}

func TestDailyTasksInterface(t *testing.T) {
	var _ provider.DailyTasks = WorkBuddy{}
}