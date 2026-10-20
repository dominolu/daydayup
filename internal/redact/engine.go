package redact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// placeholderRE matches a complete placeholder token in restored text.
var placeholderRE = regexp.MustCompile(`\{\{[A-Za-z0-9_]{1,32}_[bcdfghjkmnpqrstvwxzBCDFGHJKMNPQRSTVWXZ]{6}\}\}`)

// Stats summarizes masking and restoration for the management API.
type Stats struct {
	Requests     uint64            `json:"requests"`
	Masked       uint64            `json:"masked_values"`
	Restored     uint64            `json:"restored_values"`
	Unresolved   uint64            `json:"unresolved_placeholders"`
	ToolFindings uint64            `json:"tool_findings"`
	Entities     map[string]uint64 `json:"entities"`
	Mappings     int               `json:"mappings"`
	LastError    string            `json:"last_error,omitempty"`
}

// Engine is the redaction engine: rules + placeholder manager + durable store.
// Construction is fail-closed: an unusable engine refuses to start.
type Engine struct {
	cfg       Config
	state     rulesState
	tm        *TokenManager
	store     *Store
	maxBuffer int64

	statsMu sync.Mutex
	stats   Stats

	hookMu   sync.RWMutex
	spanHook SpanHook
}

// New builds the engine and loads the durable mapping store.
func New(cfg Config) (*Engine, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	store := OpenStore(cfg.StoreFile, cfg.StoreKeyEnv)
	// Fail fast at startup on a missing/wrong database key.
	if _, err := store.open(); err != nil {
		return nil, err
	}
	tm := newTokenManager(cfg, store)
	if err := tm.load(); err != nil {
		return nil, err
	}
	e := &Engine{cfg: cfg, tm: tm, store: store, maxBuffer: cfg.MaxRestoreBuffer, stats: Stats{Entities: map[string]uint64{}}}
	e.state.rules = buildRulesFor(cfg)
	if cfg.ToolGuard != nil {
		cfg.ToolGuard.normalize()
		e.state.toolGuard = cfg.ToolGuard
	}
	return e, nil
}

// Mask replaces sensitive spans with stable placeholders. JSON strings are
// decoded before matching and re-encoded after masking; other bytes are retained.
func (e *Engine) Mask(ctx context.Context, body []byte) ([]byte, error) {
	out, _, err := e.MaskCount(ctx, body)
	return out, err
}

// MaskCount is Mask plus the number of spans replaced, for request logging.
func (e *Engine) MaskCount(ctx context.Context, body []byte) ([]byte, int, error) {
	return e.maskCount(ctx, body, false)
}

// MaskRequestCount preserves API routing fields and media while masking content.
func (e *Engine) MaskRequestCount(ctx context.Context, body []byte) ([]byte, int, error) {
	out, n, err := e.maskCount(ctx, body, true)
	if err == nil {
		err = e.Flush()
	}
	if err != nil {
		e.recordError(err)
		return nil, 0, err
	}
	return out, n, nil
}

func (e *Engine) maskCount(ctx context.Context, body []byte, request bool) ([]byte, int, error) {
	if len(body) == 0 {
		return body, 0, nil
	}
	var out string
	var applied int
	var entityCounts map[string]uint64
	var err error
	if json.Valid(body) {
		out, applied, entityCounts, err = e.maskJSONMode(ctx, body, request, 0)
	} else {
		out, applied, entityCounts, err = e.maskText(ctx, string(body), "")
	}
	if err != nil {
		e.recordError(err)
		return nil, 0, err
	}
	e.statsMu.Lock()
	e.stats.Requests++
	e.stats.Masked += uint64(applied)
	for label, count := range entityCounts {
		e.stats.Entities[label] += count
	}
	e.statsMu.Unlock()
	if applied == 0 {
		return body, 0, nil
	}
	return []byte(out), applied, nil
}

// maskText uses an optional JSON field name to recognize credential values
// such as "password": "...", but only replaces spans inside the value itself.
func (e *Engine) maskText(ctx context.Context, value, field string) (string, int, map[string]uint64, error) {
	spans := e.collectSpans(value, field)
	if len(spans) == 0 {
		return value, 0, nil, nil
	}
	var out strings.Builder
	out.Grow(len(value))
	last := 0
	applied := 0
	entityCounts := map[string]uint64{}
	for _, s := range spans {
		// Overlap resolution keeps non-overlapping spans already sorted by start.
		if s.begin < last {
			continue
		}
		token, err := e.tm.TokenFor(s.label, value[s.begin:s.end])
		if err != nil {
			return "", 0, nil, err
		}
		e.emitSpan(ctx, SpanMasked, s.label, value[s.begin:s.end], token)
		out.WriteString(value[last:s.begin])
		out.WriteString(token)
		last = s.end
		applied++
		entityCounts[s.label]++
	}
	out.WriteString(value[last:])
	return out.String(), applied, entityCounts, nil
}

type span struct {
	label      string
	begin, end int // captured value bounds, or whole match for uncaptured rules
}

// collectSpans returns candidate value spans ordered by start position. Only
// capture-based rules see the field prefix; ordinary and custom patterns match
// the value alone, so anchored patterns still work and keys cannot be masked.
func (e *Engine) collectSpans(valueText, field string) []span {
	e.state.mu.RLock()
	rules := e.state.rules
	e.state.mu.RUnlock()
	var spans []span
	prefix := ""
	if field != "" {
		key, _ := json.Marshal(field)
		prefix = string(key) + `: "`
	}
	protected := placeholderRE.FindAllStringIndex(valueText, -1)
	appendSpan := func(label string, begin, end int, encloseTokens bool) {
		if begin < 0 || end <= begin || end > len(valueText) {
			return
		}
		for _, p := range protected {
			if begin < p[1] && end > p[0] {
				if encloseTokens && begin <= p[0] && end >= p[1] && (begin != p[0] || end != p[1]) {
					continue
				}
				return
			}
		}
		spans = append(spans, span{label: label, begin: begin, end: end})
	}
	for _, p := range protected {
		// A literal token example in source code is not an invented model token.
		// Wrap unknown input literals so a later tool call can safely reproduce
		// them, while known tokens remain stable across repeated masking.
		if _, _, ok := e.tm.Resolve(valueText[p[0]:p[1]]); !ok {
			spans = append(spans, span{label: "LITERAL", begin: p[0], end: p[1]})
		}
	}
	for _, r := range rules {
		if r.findSpans != nil {
			for _, bounds := range r.findSpans(valueText, field) {
				appendSpan(r.label, bounds[0], bounds[1], true)
			}
		}
		text, offset := valueText, 0
		if r.captureGroup > 0 {
			text, offset = prefix+valueText, len(prefix)
		}
		for _, pattern := range r.patterns {
			idx := pattern.FindAllStringSubmatchIndex(text, -1)
			for _, m := range idx {
				if len(m) < 2 || m[0] < 0 {
					continue
				}
				begin, end := m[0], m[1]
				valueBegin, valueEnd := begin, end
				if r.captureGroup > 0 {
					gi := 2 * r.captureGroup
					if gi+1 >= len(m) || m[gi] < 0 {
						continue
					}
					valueBegin, valueEnd = m[gi], m[gi+1]
				}
				if valueBegin < offset {
					continue
				}
				value := text[valueBegin:valueEnd]
				if r.semantic && !semanticValid(r.label, text, begin, end, value) {
					continue
				}
				appendSpan(r.label, valueBegin-offset, valueEnd-offset, false)
			}
		}
	}
	// Equal value spans retain rule priority (e.g. URL credentials before API keys).
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].begin != spans[j].begin {
			return spans[i].begin < spans[j].begin
		}
		return spans[i].end > spans[j].end
	})
	return spans
}

// RestoreJSON replaces known placeholders in body with JSON-escaped originals.
// Use it when body is a JSON document so restored values cannot break string
// literals (quotes, backslashes, control characters).
func (e *Engine) RestoreJSON(ctx context.Context, body []byte) ([]byte, error) {
	return e.restoreJSON(ctx, body, false, 0)
}

// RestoreToolJSON restores a complete tool argument document and rejects
// unresolvable tokens before the client can receive a completed tool call.
func (e *Engine) RestoreToolJSON(ctx context.Context, body []byte) ([]byte, error) {
	return e.restoreJSON(ctx, body, true, 0)
}

// Restore replaces known placeholders in body with their originals.
// Unknown placeholder-like text is left untouched.
func (e *Engine) Restore(ctx context.Context, body []byte) ([]byte, error) {
	return e.restore(ctx, body, false)
}

func (e *Engine) restore(ctx context.Context, body []byte, jsonEscape bool) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	text := string(body)
	loc := placeholderRE.FindAllStringIndex(text, -1)
	if len(loc) == 0 {
		return body, nil
	}
	var out strings.Builder
	out.Grow(len(text))
	last := 0
	restored, unresolved := 0, 0
	for _, l := range loc {
		token := text[l[0]:l[1]]
		label, original, ok := e.tm.Resolve(token)
		if !ok {
			unresolved++
			continue
		}
		e.emitSpan(ctx, SpanRestored, label, original, token)
		out.WriteString(text[last:l[0]])
		if jsonEscape {
			out.WriteString(jsonStringValue(original))
		} else {
			out.WriteString(original)
		}
		last = l[1]
		restored++
	}
	out.WriteString(text[last:])
	e.bump(ctx, 0, 0, uint64(restored))
	if unresolved > 0 {
		e.bumpUnresolved(ctx, uint64(unresolved))
	}
	return []byte(out.String()), nil
}

// jsonStringValue returns s encoded as a JSON string literal body (without the
// surrounding quotes).
func jsonStringValue(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	return string(b[1 : len(b)-1])
}

// RestoreSession incrementally restores a byte stream (SSE). Placeholders may
// be split across pushes; a pending tail is retained until it resolves or the
// session finishes. Unresolved trailing text is emitted unchanged.
type RestoreSession struct {
	e          *Engine
	pending    []byte
	max        int
	jsonEscape bool
}

// NewRestoreSession returns an incremental restorer bound to this engine.
func (e *Engine) NewRestoreSession() *RestoreSession {
	max := int(e.maxBuffer)
	if max < 1<<10 {
		max = 1 << 10
	}
	return &RestoreSession{e: e, max: max}
}

// NewRestoreSessionJSON returns an incremental restorer that JSON-escapes
// restored values, for tool-argument fragments inside JSON payloads.
func (e *Engine) NewRestoreSessionJSON() *RestoreSession {
	s := e.NewRestoreSession()
	s.jsonEscape = true
	return s
}

// Push consumes a chunk and returns the restorable prefix. A placeholder that
// is still incomplete stays pending until a later push completes it.
func (s *RestoreSession) Push(ctx context.Context, data []byte) ([]byte, error) {
	combined := append(append([]byte{}, s.pending...), data...)
	s.pending = nil
	var out bytes.Buffer
	for len(combined) > 0 {
		start := bytes.Index(combined, []byte("{{"))
		if start < 0 {
			end := len(combined)
			if combined[end-1] == '{' {
				end--
				s.pending = append(s.pending, '{')
			}
			out.Write(combined[:end])
			break
		}
		out.Write(combined[:start])
		combined = combined[start:]
		loc := placeholderRE.FindIndex(combined)
		if loc != nil && loc[0] == 0 {
			token := string(combined[:loc[1]])
			label, original, ok := s.e.tm.Resolve(token)
			if !ok {
				s.e.bumpUnresolved(ctx, 1)
				if s.jsonEscape {
					return nil, fmt.Errorf("redact: unresolvable placeholder in tool arguments")
				}
				out.WriteString(token)
			} else {
				s.e.emitSpan(ctx, SpanRestored, label, original, token)
				if s.jsonEscape {
					out.WriteString(jsonStringValue(original))
				} else {
					out.WriteString(original)
				}
				s.e.bump(ctx, 0, 0, 1)
			}
			combined = combined[loc[1]:]
			continue
		}
		// Only a bounded prefix of the actual token grammar is held. Literal
		// templates and nested JSON must not swallow later tokens or grow buffers.
		if len(combined) <= 44 && partialPlaceholderRE.Match(combined) {
			s.pending = append(s.pending, combined...)
			break
		}
		out.WriteByte(combined[0])
		combined = combined[1:]
	}
	return out.Bytes(), nil
}

var partialPlaceholderRE = regexp.MustCompile(`^\{\{[A-Za-z0-9_]{0,39}\}?$`)
var unfinishedPlaceholderRE = regexp.MustCompile(`^\{\{[A-Za-z0-9_]{1,32}_[bcdfghjkmnpqrstvwxzBCDFGHJKMNPQRSTVWXZ]{0,6}\}?$`)

func (s *RestoreSession) Finish(ctx context.Context) ([]byte, error) {
	tail := s.pending
	s.pending = nil
	if s.jsonEscape && unfinishedPlaceholderRE.Match(tail) {
		return nil, fmt.Errorf("redact: incomplete placeholder in tool arguments")
	}
	return tail, nil
}

// Pending reports whether the channel is withholding a possible token prefix.
func (s *RestoreSession) Pending() bool { return len(s.pending) > 0 }

// Abort drops the pending tail without emitting it.
func (s *RestoreSession) Abort() { s.pending = nil }

// Flush persists dirty mappings to the encrypted store.
func (e *Engine) Flush() error { return e.tm.Flush() }

// SetStoreReadOnly toggles the underlying database between read-only and
// writable (PRAGMA query_only). It exists for tests that simulate transient
// persistence failures; production code does not call it.
func (e *Engine) SetStoreReadOnly(ro bool) error {
	db, err := e.store.open()
	if err != nil {
		return err
	}
	value := "OFF"
	if ro {
		value = "ON"
	}
	_, err = db.SQL().Exec("PRAGMA query_only = " + value)
	return err
}

// Close flushes and releases the engine.
func (e *Engine) Close() error { return e.Flush() }

// MaxRestoreBuffer bounds complete response events and accumulated tool args.
func (e *Engine) MaxRestoreBuffer() int64 { return e.maxBuffer }

// Stats returns a consistent snapshot of counters.
func (e *Engine) Stats() Stats {
	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	out := e.stats
	out.Entities = make(map[string]uint64, len(e.stats.Entities))
	for k, v := range e.stats.Entities {
		out.Entities[k] = v
	}
	out.Mappings = e.tm.Size()
	return out
}

func (e *Engine) bump(ctx context.Context, requests, masked, restored uint64) {
	e.statsMu.Lock()
	e.stats.Requests += requests
	e.stats.Masked += masked
	e.stats.Restored += restored
	e.statsMu.Unlock()
}

func (e *Engine) bumpUnresolved(ctx context.Context, n uint64) {
	e.statsMu.Lock()
	e.stats.Unresolved += n
	e.statsMu.Unlock()
}

func (e *Engine) recordError(err error) {
	e.statsMu.Lock()
	e.stats.LastError = err.Error()
	e.statsMu.Unlock()
}

// RecordError records a runtime engine failure for the management API.
func (e *Engine) RecordError(err error) { e.recordError(err) }
