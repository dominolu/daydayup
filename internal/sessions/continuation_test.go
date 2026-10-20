package sessions

import "testing"

func TestToolContinuationOnlyUsesCompleteSuccessfulDurableRecords(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		complete      bool
		captureError  string
		want          bool
	}{
		{"success", "success", true, "", true},
		{"stream interrupted", "error", false, "", false},
		{"missing terminal", "success", false, "", false},
		{"failed response", "error", true, "", false},
		{"capture error", "success", true, "storage failure", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dbPath := testStore(t)
			c, err := s.Begin(Record{ID: "request", SessionID: "session", Provider: "sensenova", Path: "/v1/chat/completions", HistoryHash: "hash", CaptureError: tc.captureError}, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			if sid, _, _ := s.ResolveToolContinuation("sensenova", "/v1/chat/completions", "hash", []string{"call-1"}); sid != "" {
				t.Fatal("running record resolved")
			}
			if err := c.Finish(Result{Status: 200, Outcome: tc.outcome, Response: Body{Complete: tc.complete}, ToolCallIDs: []string{"call-1", "call-2"}}); err != nil {
				t.Fatal(err)
			}
			s, err = Open(dbPath, "")
			if err != nil {
				t.Fatal(err)
			}
			sid, rid, _ := s.ResolveToolContinuation("sensenova", "/v1/chat/completions", "hash", []string{"call-1", "call-2"})
			if (sid == "session" && rid == "request") != tc.want {
				t.Fatalf("wrong eligibility %q %q", sid, rid)
			}
			if sid, _, _ := s.ResolveToolContinuation("sensenova", "/v1/chat/completions", "hash", []string{"call-1", "unknown"}); sid != "" {
				t.Fatal("partial tool ID match accepted")
			}
		})
	}
}
