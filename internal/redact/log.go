package redact

import (
	"context"
)

// SpanKind distinguishes a placeholder mapping created while masking an
// outgoing request from one resolved while restoring a returning response.
type SpanKind string

const (
	// SpanMasked records an original -> placeholder mapping on the request
	// leaving the gateway.
	SpanMasked SpanKind = "masked"
	// SpanRestored records a placeholder -> original mapping on the response
	// returning to the client.
	SpanRestored SpanKind = "restored"
)

// SpanEvent describes one confidential span the engine replaced or restored:
// the matching rule label, the stable placeholder token, and the original
// value. The event carries no path or body context; request correlation is
// provided by the context passed to the masking and restoration calls.
type SpanEvent struct {
	Kind        SpanKind `json:"kind"`
	Label       string   `json:"label"`
	Placeholder string   `json:"placeholder"`
	Original    string   `json:"original"`
}

// SpanHook receives every masked or restored placeholder mapping. It runs
// synchronously on the masking/restoration path, so it must return quickly;
// a nil hook (the default) is a no-op.
type SpanHook func(context.Context, SpanEvent)

// SetSpanHook installs the hook invoked for every placeholder created during
// request masking and every placeholder resolved during response restoration.
// The hook is usually installed once at startup; replacement while requests
// are in flight is safe.
func (e *Engine) SetSpanHook(hook SpanHook) {
	e.hookMu.Lock()
	defer e.hookMu.Unlock()
	e.spanHook = hook
}

func (e *Engine) emitSpan(ctx context.Context, kind SpanKind, label, original, placeholder string) {
	if label == numericCredentialLabel {
		label = "SECRET"
	}
	e.hookMu.RLock()
	hook := e.spanHook
	e.hookMu.RUnlock()
	if hook == nil {
		return
	}
	hook(ctx, SpanEvent{Kind: kind, Label: label, Placeholder: placeholder, Original: original})
}
