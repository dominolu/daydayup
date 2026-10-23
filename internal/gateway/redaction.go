package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"safuno/internal/redact"
)

// RedactionError reports a redaction engine initialization failure so the
// gateway can refuse to start (fail-closed) instead of serving unmasked.
func (s *Server) RedactionError() error { return s.redactErr }

func (s *Server) redactFailClosed() bool {
	cfg := s.redactionConfig()
	return cfg == nil || cfg.FailClosedValue
}

func (s *Server) redactionConfig() *redact.Config {
	s.redactionMu.RLock()
	defer s.redactionMu.RUnlock()
	return s.Config.Redaction
}

// FlushRedaction is called periodically and after all HTTP handlers shut down.
func (s *Server) FlushRedaction() error {
	if s.redact == nil {
		return nil
	}
	return s.redact.Flush()
}

type redactionInputError struct {
	status  int
	message string
}

func (e *redactionInputError) Error() string { return e.message }

func (s *Server) maskRequestBody(r *http.Request, body []byte) (out []byte, spans int, resultErr error) {
	decodedGzip := false
	defer func() {
		if decodedGzip && resultErr == nil {
			r.Header.Del("Content-Encoding")
			r.Header.Del("Content-Length")
		}
	}()
	encoding := strings.TrimSpace(strings.ToLower(r.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" {
		if encoding != "gzip" {
			return nil, 0, &redactionInputError{415, "unsupported request content encoding"}
		}
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, 0, &redactionInputError{400, "invalid gzip body"}
		}
		decoded, err := io.ReadAll(io.LimitReader(zr, s.Config.MaxRequestBytes+1))
		_ = zr.Close()
		if err != nil {
			return nil, 0, &redactionInputError{400, "invalid gzip body"}
		}
		if int64(len(decoded)) > s.Config.MaxRequestBytes {
			return nil, 0, &redactionInputError{413, "decoded request exceeds max_request_bytes"}
		}
		body = decoded
		decodedGzip = true
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" || strings.HasSuffix(ct, "+json") {
		if len(body) > 0 && !json.Valid(body) {
			return nil, 0, &redactionInputError{400, "invalid JSON body"}
		}
	}
	if ct == "application/x-www-form-urlencoded" {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, 0, &redactionInputError{400, "invalid form body"}
		}
		count := 0
		for key, list := range values {
			if key == "model" {
				continue
			}
			for i, value := range list {
				// A JSON object supplies credential-field context and safely
				// handles values containing separators, quotes or percent escapes.
				b, _ := json.Marshal(map[string]string{key: value})
				masked, n, err := s.redact.MaskCount(r.Context(), b)
				if err != nil {
					return nil, 0, err
				}
				var field map[string]string
				if err := json.Unmarshal(masked, &field); err != nil {
					return nil, 0, err
				}
				list[i] = field[key]
				count += n
			}
		}
		if count == 0 {
			return body, 0, nil
		}
		if err := s.redact.Flush(); err != nil {
			return nil, 0, err
		}
		return []byte(values.Encode()), count, nil
	}
	return s.redact.MaskRequestCount(r.Context(), body)
}

func (s *Server) redactRecordError(err error) {
	if s.redact != nil {
		s.redact.RecordError(err)
	}
}

type redactionGzipBody struct {
	*gzip.Reader
	source io.ReadCloser
}

func (b *redactionGzipBody) Close() error { _ = b.Reader.Close(); return b.source.Close() }

func decodeRedactionResponse(resp *http.Response) error {
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if encoding == "" || encoding == "identity" {
		return nil
	}
	if encoding != "gzip" {
		return fmt.Errorf("unsupported upstream response content encoding")
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("invalid upstream gzip response")
	}
	resp.Body = &redactionGzipBody{Reader: zr, source: resp.Body}
	resp.Header.Del("Content-Encoding")
	return nil
}

// maskableContentType decides whether a request body can be masked. JSON and
// text bodies are processed; multipart and opaque binary payloads (images,
// audio) are passed through untouched so masking can never corrupt them.
func maskableContentType(ct string) bool {
	if ct == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return true
	}
	switch {
	case mt == "application/json", strings.HasSuffix(mt, "+json"):
		return true
	case strings.HasPrefix(mt, "text/"):
		return true
	case mt == "application/x-www-form-urlencoded":
		return true
	default:
		return false
	}
}

// copyRestored buffers an upstream JSON response and writes the restored
// version to w. Over-limit, unreadable or unrestorable responses abort before
// writing body bytes, so no truncated response can look like a successful one.
func (s *Server) copyRestored(ctx context.Context, w http.ResponseWriter, r io.Reader, isJSON bool) error {
	limit := s.redact.MaxRestoreBuffer()
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		err := fmt.Errorf("response exceeds redaction restore buffer")
		s.redactRecordError(err)
		return err
	}
	var restored []byte
	var rerr error
	if isJSON {
		restored, rerr = s.redact.RestoreJSON(ctx, data)
	} else {
		restored, rerr = s.redact.Restore(ctx, data)
	}
	if rerr != nil {
		s.redactRecordError(rerr)
		slog.Warn("redaction restore failed", "error", rerr)
		return rerr
	}
	if isJSON {
		s.logToolFindings(s.redact.CheckToolJSON(restored))
	}
	n, werr := w.Write(restored)
	if werr == nil && n != len(restored) {
		return io.ErrShortWrite
	}
	return werr
}

// logToolFindings records dangerous tool-call observations (observe mode:
// the calls still reach the client, the finding is only logged and counted).
func (s *Server) logToolFindings(findings []redact.ToolFinding) {
	if len(findings) == 0 {
		return
	}
	rules := make([]string, 0, len(findings))
	for _, f := range findings {
		rules = append(rules, f.Label)
	}
	slog.Warn("dangerous tool call observed (redaction tool_guard)", "rules", rules)
}

// logRedactionSpan records one placeholder mapping created by request masking
// (kind=masked, original -> placeholder) or resolved by response restoration
// (kind=restored, placeholder -> original). The original value is passed
// through the request sanitizer so account keys and known credential formats
// never appear verbatim, and is truncated to the sanitizer's preview length.
func (s *Server) logRedactionSpan(ctx context.Context, ev redact.SpanEvent) {
	attrs := []any{
		"event", "redaction_span",
		"kind", string(ev.Kind),
		"label", ev.Label,
		"placeholder", ev.Placeholder,
		"original", s.logSanitizer()(ev.Original),
	}
	rec := redactionSpanRecord{Time: time.Now(), Kind: string(ev.Kind), Label: ev.Label, Placeholder: ev.Placeholder, Original: ev.Original}
	if t := traceFromCtx(ctx); t != nil {
		attrs = append(attrs, "request_id", t.ID)
		rec.RequestID = t.ID
	}
	if s.redactionSpans != nil {
		s.redactionSpans.add(rec)
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("redaction span", attrs...)
}

// redactionAdmin serves GET /admin/v1/redaction with the live stats and the
// effective configuration of the masking layer.
func (s *Server) redactionAdmin(w http.ResponseWriter, r *http.Request) {
	cfg := s.redactionConfig()
	if s.redact == nil || cfg == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	toolGuard := map[string]any{"enabled": false}
	if cfg.ToolGuard != nil {
		toolGuard = map[string]any{"enabled": cfg.ToolGuard.Enabled, "mode": cfg.ToolGuard.Mode}
	}
	ruleCatalog := map[string]string{}
	for _, rule := range redact.BuiltinCatalog() {
		ruleCatalog[rule] = redact.BuiltinDescriptions()[rule]
	}
	writeJSON(w, 200, map[string]any{
		"enabled":            true,
		"scope":              cfg.ScopeValue,
		"store_file":         cfg.StoreFile,
		"fail_closed":        cfg.FailClosedValue,
		"disabled_rules":     cfg.DisabledRules,
		"custom_words":       cfg.CustomWords,
		"max_mappings":       cfg.MaxMappings,
		"max_token_length":   cfg.MaxTokenLength,
		"max_restore_buffer": cfg.MaxRestoreBuffer,
		"token_ttl_seconds":  cfg.TokenTTLSeconds,
		"tool_guard":         toolGuard,
		"rule_catalog":       ruleCatalog,
		"stats":              s.redact.Stats(),
	})
}

// redactionUpdateRules hot-reloads the rule table (disabled labels, custom
// words, tool guard) on the running engine without a restart.
func (s *Server) redactionUpdateRules(w http.ResponseWriter, r *http.Request) {
	s.redactionMu.Lock()
	defer s.redactionMu.Unlock()
	if s.redact == nil || s.Config.Redaction == nil {
		apiError(w, 400, "redaction_disabled", "redaction engine is not enabled")
		return
	}
	var patch struct {
		DisabledRules []string            `json:"disabled_rules"`
		CustomWords   map[string][]string `json:"custom_words"`
		ToolGuard     *redact.ToolGuard   `json:"tool_guard"`
	}
	if !decodeJSON(w, r, &patch) {
		return
	}
	next := *s.Config.Redaction
	if patch.DisabledRules != nil {
		next.DisabledRules = patch.DisabledRules
	}
	if patch.CustomWords != nil {
		next.CustomWords = patch.CustomWords
	}
	if patch.ToolGuard != nil {
		next.ToolGuard = patch.ToolGuard
	}
	if err := next.Normalize(); err != nil {
		apiError(w, 400, "invalid_rules", err.Error())
		return
	}
	if err := s.saveRedactionRules(next); err != nil {
		apiError(w, 500, "redaction_rules_save_failed", "redaction rules could not be saved; running rules were retained")
		return
	}
	if err := s.redact.Reconfigure(next); err != nil {
		apiError(w, 400, "invalid_rules", err.Error())
		return
	}
	s.Config.Redaction = &next
	writeJSON(w, 200, map[string]any{"ok": true})
}

// Preserve raw configuration fields and environment-variable references. Never
// serialize the effective Config, which may contain resolved account secrets.
func (s *Server) saveRedactionRules(cfg redact.Config) error {
	if s.Config.SourcePath == "" {
		return nil
	}
	path := s.Config.SourcePath
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	doc["redaction"], err = json.Marshal(cfg)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".redaction-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(encoded); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
