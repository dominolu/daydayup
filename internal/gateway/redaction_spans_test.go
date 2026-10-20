package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRedactionSpanLogRing(t *testing.T) {
	l := newRedactionSpanLog(4)
	for i := 0; i < 6; i++ {
		l.add(redactionSpanRecord{Time: time.Unix(int64(i+1), 0), Kind: "masked", Label: "EMAIL", Placeholder: "p", Original: "o", RequestID: "r"})
	}
	recs := l.recent(0, "", "", "")
	if len(recs) != 4 {
		t.Fatalf("ring capacity: got %d records, want 4", len(recs))
	}
	if recs[0].Time.Unix() != 6 || recs[3].Time.Unix() != 3 {
		t.Fatalf("newest-first order: %+v", recs)
	}
	if got := l.recent(2, "", "", ""); len(got) != 2 || got[0].Time.Unix() != 6 || got[1].Time.Unix() != 5 {
		t.Fatalf("limit: %+v", got)
	}
	if got := l.recent(0, "restored", "", ""); len(got) != 0 {
		t.Fatalf("kind filter must exclude restored: %+v", got)
	}
	l.add(redactionSpanRecord{Time: time.Unix(7, 0), Kind: "restored", Label: "PHONE", Placeholder: "q", Original: "w", RequestID: "r2"})
	if got := l.recent(0, "", "PHONE", ""); len(got) != 1 || got[0].Kind != "restored" {
		t.Fatalf("label filter: %+v", got)
	}
	if got := l.recent(0, "", "", "r2"); len(got) != 1 || got[0].RequestID != "r2" {
		t.Fatalf("request_id filter: %+v", got)
	}
}

func TestRedactionSpansAdminRecordsRealRequest(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			return
		}
		echo := ""
		if len(req.Messages) > 0 {
			echo = req.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": echo}, "finish_reason": "stop"}}})
	})
	s, _ := redactedGateway(t, upstream)

	body := `{"model":"m","messages":[{"role":"user","content":"电话 13800138000，邮箱 user@example.com"}]}`
	rec := call(s, "POST", "/v1/chat/completions", body)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	requestID := rec.Header().Get("X-Request-ID")

	get := func(query string) map[string]any {
		r := httptest.NewRequest("GET", "/admin/v1/redaction/spans"+query, nil)
		r.Header.Set("Authorization", "Bearer "+s.Config.AdminKey)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("spans endpoint status %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("spans json: %v", err)
		}
		return resp
	}

	raw := get("")
	data, _ := raw["data"].([]any)
	if len(data) < 4 {
		t.Fatalf("expected at least 4 span records (2 masked + 2 restored), got %d: %s", len(data), raw)
	}
	var masked, restored []map[string]any
	for _, item := range data {
		span, _ := item.(map[string]any)
		if span["request_id"] != requestID {
			t.Fatalf("span missing request correlation: %v", span)
		}
		switch span["kind"] {
		case "masked":
			masked = append(masked, span)
		case "restored":
			restored = append(restored, span)
		}
	}
	if len(masked) != 2 || len(restored) != 2 {
		t.Fatalf("expected 2 masked and 2 restored spans, got %d/%d: %s", len(masked), len(restored), raw)
	}
	byPlaceholder := map[string]string{}
	for _, span := range masked {
		label, _ := span["label"].(string)
		placeholder, _ := span["placeholder"].(string)
		original, _ := span["original"].(string)
		if label != "PHONE" && label != "EMAIL" {
			t.Fatalf("unexpected label: %v", span)
		}
		if placeholder == "" || original == "" || placeholder == original {
			t.Fatalf("invalid mapping: %v", span)
		}
		byPlaceholder[placeholder] = original
	}
	for _, span := range restored {
		placeholder, _ := span["placeholder"].(string)
		original, _ := span["original"].(string)
		if byPlaceholder[placeholder] != original {
			t.Fatalf("restored mapping differs: %v vs %q", span, byPlaceholder[placeholder])
		}
	}
	// kind filter narrows the trace.
	only := get("?kind=masked")
	onlyData, _ := only["data"].([]any)
	for _, item := range onlyData {
		if kind, _ := item.(map[string]any)["kind"].(string); kind != "masked" {
			t.Fatalf("kind filter leaked %q", kind)
		}
	}
	// unauthenticated access is rejected by the admin guard.
	r := httptest.NewRequest("GET", "/admin/v1/redaction/spans", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("spans endpoint requires admin key, got %d", w.Code)
	}
}
