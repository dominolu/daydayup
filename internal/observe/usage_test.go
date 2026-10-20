package observe

import (
	"strings"
	"testing"
)

func TestUsageProtocols(t *testing.T) {
	for _, tt := range []struct {
		name, body                   string
		sse                          bool
		input, output, total, cached int64
	}{
		{"chat", `{"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":2}}}`, false, 3, 4, 7, 2},
		{"responses", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":9,\"total_tokens\":14}}}\n\n", true, 5, 9, 14, 0},
		{"anthropic", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":10,\"cache_creation_input_tokens\":2,\"output_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\ndata: {\"type\":\"message_stop\"}\n\n", true, 15, 4, 19, 10},
		{"chat-repeated-usage", "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\r\n\r\ndata: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\r\n\r\ndata: [DONE]\r\n\r\n", true, 3, 4, 7, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := NewObserver(tt.sse, false)
			for _, b := range []byte(tt.body) {
				_, _ = o.Write([]byte{b})
			}
			o.Done()
			u := o.usage
			if !u.Known || u.Input != tt.input || u.Output != tt.output || u.Total != tt.total || u.Cached != tt.cached {
				t.Fatalf("usage: %+v", u)
			}
			if tt.sse && !o.terminal {
				t.Fatal("missing completion marker")
			}
		})
	}
}

func TestSSETerminalRequiresACompleteUnambiguousEvent(t *testing.T) {
	for _, wire := range []string{"data: [DONE]", "data: [DONE]\n", "data: {\"type\":\"message_stop\"}\n", "data: {\"type\":\"response.completed\"}\n"} {
		o := NewObserver(true, false)
		o.Write([]byte(wire))
		o.Done()
		if o.terminal {
			t.Fatal("EOF dispatched an unfinished SSE event")
		}
	}
	o := NewObserver(true, false)
	o.Write([]byte("data: " + strings.Repeat("x", observationLimit+1) + "\ndata: [DONE]\n\n"))
	if o.terminal || !o.observationGap {
		t.Fatal("a fragment of an oversized event became a terminal marker")
	}
	o.Write([]byte("data: [DONE]\n\n"))
	if !o.terminal {
		t.Fatal("a later independent complete terminal event was missed")
	}
}
func TestUnknownAndErrorUsage(t *testing.T) {
	o := NewObserver(false, false)
	_, _ = o.Write([]byte(`{"choices":[]}`))
	o.Done()
	if o.usage.Known {
		t.Fatal("missing usage fabricated as zero")
	}
	o = NewObserver(true, false)
	_, _ = o.Write([]byte("data: {\"type\":\"error\",\"error\":{\"message\":\"bad\"}}\n\n"))
	o.Done()
	if !o.failed {
		t.Fatal("stream error missed")
	}
}

func TestUsageReportedFieldsDistinguishUnknownFromZero(t *testing.T) {
	o := NewObserver(false, false)
	o.Write([]byte(`{"usage":{"input_tokens":0,"total_tokens":12}}`))
	o.Done()
	if !reported(o.reported, "input_tokens") || !reported(o.reported, "total_tokens") || reported(o.reported, "output_tokens") || reported(o.reported, "cached_input_tokens") {
		t.Fatalf("incorrect reported fields %v", o.reported)
	}
}
