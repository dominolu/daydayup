package redact

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// spanRecorder collects SpanEvents delivered by the engine hook so tests can
// assert exactly which placeholders were created and resolved.
type spanRecorder struct {
	mu     sync.Mutex
	events []SpanEvent
}

func (r *spanRecorder) hook(_ context.Context, ev SpanEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *spanRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

func (r *spanRecorder) all() []SpanEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SpanEvent, len(r.events))
	copy(out, r.events)
	return out
}

func TestMaskEmitsOneSpanPerValue(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomWords = map[string][]string{"CUSTOM": {"alpha-project"}}
	e := newTestEngine(t, cfg)
	rec := &spanRecorder{}
	e.SetSpanHook(rec.hook)

	out, n, err := e.MaskCount(context.Background(), []byte(`代号 alpha-project，邮箱 user@example.com`))
	if err != nil || n != 2 {
		t.Fatalf("mask: n=%d err=%v out=%s", n, err, out)
	}
	events := rec.all()
	if len(events) != 2 {
		t.Fatalf("expected 2 masked span events, got %d: %+v", len(events), events)
	}
	custom, email := events[0], events[1]
	if custom.Kind != SpanMasked || custom.Label != "CUSTOM" || custom.Original != "alpha-project" {
		t.Fatalf("custom word event: %+v", custom)
	}
	if !strings.HasPrefix(custom.Placeholder, "{{CUSTOM_") || !strings.HasSuffix(custom.Placeholder, "}}") {
		t.Fatalf("custom placeholder malformed: %+v", custom)
	}
	if email.Kind != SpanMasked || email.Label != "EMAIL" || email.Original != "user@example.com" {
		t.Fatalf("email event: %+v", email)
	}
	if email.Placeholder == email.Original || !strings.HasPrefix(email.Placeholder, "{{EMAIL_") {
		t.Fatalf("email placeholder must differ from the original span: %+v", email)
	}
	if !strings.Contains(string(out), custom.Placeholder) || !strings.Contains(string(out), email.Placeholder) {
		t.Fatalf("masked output missing placeholders: %s", out)
	}
	if strings.Contains(string(out), "alpha-project") || strings.Contains(string(out), "user@example.com") {
		t.Fatalf("masked output leaks originals: %s", out)
	}
}

func TestRemaskOfKnownPlaceholderDoesNotEmit(t *testing.T) {
	cfg := testConfig(t)
	cfg.CustomWords = map[string][]string{"CUSTOM": {"alpha-project"}}
	e := newTestEngine(t, cfg)
	rec := &spanRecorder{}
	e.SetSpanHook(rec.hook)

	first := mustMask(t, e, "代号 alpha-project")
	if len(rec.all()) != 1 {
		t.Fatalf("first mask events: %+v", rec.all())
	}
	rec.reset()
	second := mustMask(t, e, first)
	if second != first {
		t.Fatalf("masking is not idempotent: %s -> %s", first, second)
	}
	if events := rec.all(); len(events) != 0 {
		t.Fatalf("re-masking a known placeholder must not mint spans: %+v", events)
	}
}

func TestRestoreEmitsSpan(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	rec := &spanRecorder{}
	e.SetSpanHook(rec.hook)

	token := mustMask(t, e, "user@example.com")
	rec.reset()
	out, err := e.Restore(context.Background(), []byte("联系 "+token))
	if err != nil || string(out) != "联系 user@example.com" {
		t.Fatalf("restore: %q err=%v", out, err)
	}
	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("expected 1 restored event, got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Kind != SpanRestored || ev.Label != "EMAIL" || ev.Placeholder != token || ev.Original != "user@example.com" {
		t.Fatalf("restored event: %+v", ev)
	}
}

func TestRestoreJSONEmitsSpan(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	rec := &spanRecorder{}
	e.SetSpanHook(rec.hook)

	token := mustMask(t, e, "user@example.com")
	rec.reset()
	out, err := e.RestoreJSON(context.Background(), []byte(`{"content":"联系 `+token+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	events := rec.all()
	if len(events) != 1 || events[0].Kind != SpanRestored || events[0].Placeholder != token || events[0].Original != "user@example.com" {
		t.Fatalf("json restore events: %+v out=%s", events, out)
	}
}

func TestRestoreSessionPushEmitsOnlyOnCompletion(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	rec := &spanRecorder{}
	e.SetSpanHook(rec.hook)

	token := mustMask(t, e, "user@example.com")
	rec.reset()
	split := len(token) / 2
	s := e.NewRestoreSession()
	first, err := s.Push(context.Background(), []byte(token[:split]))
	if err != nil {
		t.Fatal(err)
	}
	if events := rec.all(); len(events) != 0 {
		t.Fatalf("partial placeholder must not resolve yet: %+v", events)
	}
	if s.Pending() != true {
		t.Fatal("session must hold the incomplete placeholder prefix")
	}
	second, err := s.Push(context.Background(), []byte(token[split:]))
	if err != nil {
		t.Fatal(err)
	}
	if string(first)+string(second) != "user@example.com" {
		t.Fatalf("stream restore mismatch: %q + %q", first, second)
	}
	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("expected 1 restored event after completion, got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Kind != SpanRestored || ev.Label != "EMAIL" || ev.Placeholder != token || ev.Original != "user@example.com" {
		t.Fatalf("stream restored event: %+v", ev)
	}
}

func TestUnresolvedPlaceholderDoesNotEmit(t *testing.T) {
	e := newTestEngine(t, testConfig(t))
	rec := &spanRecorder{}
	e.SetSpanHook(rec.hook)

	out, err := e.Restore(context.Background(), []byte("未知 {{LITERAL_qwerzx}} 保留"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "未知 {{LITERAL_qwerzx}} 保留" {
		t.Fatalf("unknown placeholder must pass through untouched: %s", out)
	}
	if events := rec.all(); len(events) != 0 {
		t.Fatalf("unknown placeholder must not emit a span: %+v", events)
	}
}
