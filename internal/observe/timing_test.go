package observe

import (
	"math"
	"testing"
	"time"
)

func TestTimingMeasuresGeneratedDeltasAndExcludesTrailingUsage(t *testing.T) {
	start := time.Unix(1000, 0)
	now := start
	o := NewObserver(true, false)
	o.now = func() time.Time { return now }
	send := func(ms int64, body string) {
		now = start.Add(time.Duration(ms) * time.Millisecond)
		o.Write([]byte("data: " + body + "\n\n"))
	}
	send(100, `{"choices":[{"delta":{"role":"assistant","content":""}}]}`)
	send(500, `{"usage":{"prompt_tokens":30,"completion_tokens":0}}`)
	send(1200, `{"choices":[{"delta":{"reasoning_content":"reasoning"}}]}`)
	send(1400, `{"choices":[{"delta":{"content":"answer"}}]}`)
	send(2000, `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`)
	send(10000, `{"usage":{"prompt_tokens":30,"completion_tokens":103,"total_tokens":133}}`)
	send(11000, `[DONE]`)
	o.Done()
	timing := o.Timing(start, true)
	if timing.TTFTMS == nil || *timing.TTFTMS != 1200 || timing.GenerationMS == nil || *timing.GenerationMS != 800 || timing.GenerationTokens != 103 {
		t.Fatalf("wrong timing %+v", timing)
	}
	if rate := float64(timing.GenerationTokens) * 1000 / (*timing.GenerationMS); math.Abs(rate-128.75) > .0001 {
		t.Fatal(rate)
	}
}
func TestTimingDoesNotInventSpeedForBufferedOrIncompleteResponses(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, tc := range []struct {
		name          string
		sse, complete bool
		wire          string
	}{
		{"buffered-stream", true, true, "data: {\"choices\":[{\"delta\":{\"content\":\"A\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"B\"}}]}\n\ndata: {\"usage\":{\"completion_tokens\":20}}\n\ndata: [DONE]\n\n"},
		{"non-stream", false, true, `{"choices":[{"message":{"content":"answer"}}],"usage":{"completion_tokens":20}}`},
		{"no-output", true, true, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n"},
		{"interrupted", true, false, "data: {\"choices\":[{\"delta\":{\"content\":\"A\"}}]}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := NewObserver(tc.sse, false)
			o.now = func() time.Time { return start.Add(time.Second) }
			o.Write([]byte(tc.wire))
			o.Done()
			timing := o.Timing(start, tc.complete)
			if timing.GenerationMS != nil || timing.GenerationTokens != 0 {
				t.Fatal("fabricated speed")
			}
			if (!tc.sse || tc.name == "no-output") && timing.TTFTMS != nil {
				t.Fatal("fabricated TTFT")
			}
		})
	}
}
func TestTimingSupportsResponsesAndAnthropic(t *testing.T) {
	for _, body := range []string{
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
		`{"type":"response.function_call_arguments.delta","delta":"{}"}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`,
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thinking"}}`,
		`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}`,
	} {
		o := NewObserver(true, false)
		o.Write([]byte("data: " + body + "\n\n"))
		o.Done()
		if o.firstGenerated.IsZero() {
			t.Errorf("content not recognized %s", body)
		}
	}
	o := NewObserver(true, false)
	o.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
	if !o.firstGenerated.IsZero() {
		t.Fatal("metadata counted as generation")
	}
}
func TestCacheCreationAndDeepSeekCacheFields(t *testing.T) {
	o := NewObserver(false, false)
	o.Write([]byte(`{"usage":{"input_tokens":5,"cache_creation_input_tokens":10,"output_tokens":2}}`))
	o.Done()
	if o.usage.Input != 15 || o.usage.Total != 17 || reported(o.reported, "cached_input_tokens") {
		t.Fatalf("cache creation accounting %+v", o.usage)
	}
	o = NewObserver(false, false)
	o.Write([]byte(`{"usage":{"prompt_tokens":30,"completion_tokens":2,"prompt_cache_hit_tokens":20}}`))
	o.Done()
	if o.usage.Cached != 20 || !reported(o.reported, "cached_input_tokens") {
		t.Fatal("cache hit field not observed")
	}
}
