package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

// streamStopDiagnostic names why a response body ended before its protocol
// terminal event. Precedence matters:
//
//   - a client that went away is not an upstream failure;
//   - our own request deadline is not a truncated upstream stream — the
//     upstream was still streaming when the gateway gave up on it, and
//     reporting it as "response_incomplete" hides a configuration limit
//     behind an upstream symptom;
//   - everything else is an upstream stop (clean EOF, reset, GOAWAY, …).
func streamStopDiagnostic(r *http.Request, ctx context.Context) diagnostic {
	switch {
	case r.Context().Err() != nil:
		return diagnostic{Layer: "client", Reason: "client_cancelled", Code: "client_cancelled", Message: "client cancelled the request"}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return diagnostic{Layer: "timeout", Reason: "upstream_timeout", Code: "upstream_timeout", Message: "gateway request timeout exceeded while reading the upstream response"}
	default:
		return diagnostic{Layer: "stream", Reason: "response_incomplete", Message: "response ended before completion"}
	}
}

// streamErrorClass reduces a body-read error to a stable token so truncation
// can be triaged without storing driver or network messages per fingerprint.
func streamErrorClass(err error) string {
	switch {
	case err == nil:
		return "upstream_closed"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, io.EOF):
		return "eof"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "network_timeout"
		}
		return "network_error"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "network_error"
	}
	if strings.Contains(err.Error(), "GOAWAY") {
		return "http2_goaway"
	}
	return "other"
}

// streamErrorMessage is the sanitized (by the caller) read error text. It is
// empty when the body simply ended without a terminal event.
func streamErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
