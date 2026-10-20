package sessions

import (
	"bytes"
	"math"
	"os"
	"testing"
)

func closeEnough(a, b float64) bool { return math.Abs(a-b) < .00001 }
func TestInsightAggregationIsWeightedAndCacheCoverageIsExplicit(t *testing.T) {
	// Different stream speeds: use token/time weighting, not mean TPS.
	r := Record{ID: "r1", Attempts: []Attempt{
		{DurationMS: 3000, Timing: &Timing{Streaming: true, TTFTMS: floatPointer(1000), GenerationMS: floatPointer(1000), GenerationTokens: 100}, Usage: Usage{Input: 100, Cached: 60, Output: 100, Total: 200, Known: true, Reported: []string{"input_tokens", "cached_input_tokens", "output_tokens", "total_tokens"}}},
		{DurationMS: 8000, Timing: &Timing{Streaming: true, TTFTMS: floatPointer(2000), GenerationMS: floatPointer(4000), GenerationTokens: 200}, Usage: Usage{Input: 50, Cached: 30, Output: 200, Total: 250, Known: true, Reported: []string{"input_tokens", "cached_input_tokens", "output_tokens", "total_tokens"}}},
		{DurationMS: 500, Usage: Usage{Input: 10, Output: 5, Total: 15, Known: true, Reported: []string{"input_tokens", "output_tokens", "total_tokens"}}},
	}}
	r = decorate(r, nil)
	p, d := r.Performance, r.TokenDetails
	if *p.ModelTimeMS != 11500 || *p.TTFTMS != 1500 || *p.TPS != 60 || p.TPSSamples != 2 || p.TTFTSamples != 2 {
		t.Fatalf("bad timing %+v", p)
	}
	if *d.CacheHitRate != 60 || *d.UncachedInput != 60 || d.CacheSamples != 2 || d.CacheUnknownAttempts != 1 {
		t.Fatalf("bad cache coverage %+v", d)
	}
	summary := Summary{}
	mergeInsights(&summary, r)
	another := decorate(Record{Attempts: []Attempt{{DurationMS: 1000, Timing: &Timing{TTFTMS: floatPointer(600), GenerationMS: floatPointer(500), GenerationTokens: 50}}}}, nil)
	mergeInsights(&summary, another)
	if !closeEnough(*summary.Performance.TTFTMS, 1200) || !closeEnough(*summary.Performance.TPS, 350000.0/5500) {
		t.Fatalf("incorrect session weighting %+v", summary.Performance)
	}
}
func TestUnknownCacheIsNotZeroAndInvalidCacheCannotExceed100Percent(t *testing.T) {
	for _, u := range []Usage{
		{Input: 100, Cached: 0, Known: true, Reported: []string{"input_tokens"}},
		{Input: 100, Cached: 110, Known: true, Reported: []string{"input_tokens", "cached_input_tokens"}},
		{Input: 100, Cached: 30, Known: true},
	} {
		r := decorate(Record{Attempts: []Attempt{{Usage: u}}}, nil)
		if r.TokenDetails.CacheHitRate != nil || r.TokenDetails.UncachedInput != nil {
			t.Fatalf("unknown cache invented %+v", r.TokenDetails)
		}
	}
	r := decorate(Record{Attempts: []Attempt{{Usage: Usage{Input: 100, Cached: 0, Known: true, Reported: []string{"input_tokens", "cached_input_tokens"}}}}}, nil)
	if r.TokenDetails.CacheHitRate == nil || *r.TokenDetails.CacheHitRate != 0 || *r.TokenDetails.UncachedInput != 100 {
		t.Fatal("reported zero treated as unknown")
	}
}
func TestReportedToolDurationsAreEncryptedIdempotentAndSurviveRestart(t *testing.T) {
	s, dbPath := testStore(t)
	record(t, s, "r1", nil)
	record(t, s, "r2", nil)
	first := ToolEvent{ID: "call-1", RequestID: "r1", Name: "private-tool-name", DurationMS: 300, Status: "success"}
	created, err := s.ReportTool("session-1", first)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if created, err = s.ReportTool("session-1", first); err != nil || created {
		t.Fatal("duplicate event not idempotent")
	}
	first.DurationMS = 500
	if _, err = s.ReportTool("session-1", first); err != ErrToolConflict {
		t.Fatal("conflicting event overwrote value")
	}
	if _, err = s.ReportTool("session-1", ToolEvent{ID: "call-2", RequestID: "r2", DurationMS: 200}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ToolEvent{{ID: "../bad", RequestID: "r1"}, {ID: "bad", RequestID: "r1", DurationMS: -1}, {ID: "bad", RequestID: "r1", DurationMS: math.Inf(1)}} {
		if _, err = s.ReportTool("session-1", bad); err != ErrInvalidTool {
			t.Fatal("invalid tool accepted")
		}
	}
	if _, err = s.ReportTool("session-1", ToolEvent{ID: "other", RequestID: "unknown"}); !os.IsNotExist(err) {
		t.Fatal("tool attached to missing request")
	}
	// Tool metadata is an encrypted row: the database must never hold it plaintext.
	db, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(db, []byte("private-tool-name")) {
		t.Fatal("tool metadata leaked plaintext")
	}
	s, err = Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	sum, _, _, _ := s.Detail("session-1", 0, 30)
	if sum.Performance.ToolTimeMS == nil || *sum.Performance.ToolTimeMS != 500 || sum.Performance.ToolCalls != 2 || sum.Usage.Total != 12 || sum.Requests != 2 {
		t.Fatalf("tool totals %+v", sum)
	}
	r, _ := s.Record("session-1", "r1")
	if *r.Performance.ToolTimeMS != 300 || r.Performance.ToolCalls != 1 || r.Performance.ToolTimeSource != "client_reported" {
		t.Fatal("per-request tool isolation lost")
	}
	tools, total, _ := s.Tools("session-1", "", 0, 1)
	if total != 2 || len(tools) != 1 || tools[0].Source != "client_reported" {
		t.Fatal("tool pagination/source")
	}
	fresh := decorate(Record{Attempts: []Attempt{{DurationMS: 200}}}, nil)
	if fresh.Performance.ToolTimeMS != nil || fresh.Performance.TTFTMS != nil || fresh.Performance.TPS != nil {
		t.Fatal("historical timing invented")
	}
}
