// Package redact implements a privacy masking layer modeled on the silencer
// gateway: sensitive content in client requests is replaced with stable
// placeholders before forwarding, and model responses are restored before
// they reach the client. Secrets (API keys, tokens, credentials) and PII
// (phones, emails, IDs, cards, IPs) share the same rule pipeline.
package redact

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Defaults for the redaction layer.
const (
	DefaultScope            = "default"
	DefaultStoreFile        = "data/redaction.map.enc"
	DefaultStoreKeyEnv      = "GATEWAY_REDACTION_STORE_KEY"
	DefaultMaxMappings      = 100000
	DefaultMaxTokenLength   = 4096
	DefaultMaxRestoreBuffer = int64(32 << 20)
	DefaultTokenTTL         = 30 * 24 * 3600 // seconds
)

// Config is embedded in the gateway configuration as "redaction".
// A nil or disabled config leaves the gateway fully transparent.
type Config struct {
	Enabled          bool                `json:"enabled"`
	Scope            string              `json:"scope,omitempty"`
	StoreFile        string              `json:"store_file,omitempty"`
	StoreKeyEnv      string              `json:"store_key_env,omitempty"`
	DisabledRules    []string            `json:"disabled_rules,omitempty"`
	CustomWords      map[string][]string `json:"custom_words,omitempty"`
	MaxMappings      int                 `json:"max_mappings,omitempty"`
	MaxTokenLength   int                 `json:"max_token_length,omitempty"`
	MaxRestoreBuffer int64               `json:"max_restore_buffer,omitempty"`
	TokenTTLSeconds  int                 `json:"token_ttl_seconds,omitempty"`
	FailClosed       *bool               `json:"fail_closed,omitempty"`
	ToolGuard        *ToolGuard          `json:"tool_guard,omitempty"`
	ScopeValue       string              `json:"-"`
	FailClosedValue  bool                `json:"-"`
}

// Normalize fills defaults and validates the config. It returns an error for
// invalid scopes, empty store paths, unknown disabled rule labels, and
// malformed custom regex patterns.
func (c *Config) Normalize() error {
	if c.Scope == "" {
		c.Scope = DefaultScope
	}
	c.ScopeValue = c.Scope
	if c.ScopeValue == "" || len(c.ScopeValue) > 128 || strings.ContainsAny(c.ScopeValue, "\r\n\x00{}") {
		return errors.New("redaction: scope must be 1-128 characters without control characters or braces")
	}
	if c.StoreFile == "" {
		c.StoreFile = DefaultStoreFile
	}
	if c.StoreKeyEnv == "" {
		c.StoreKeyEnv = DefaultStoreKeyEnv
	}
	if c.MaxMappings <= 0 {
		c.MaxMappings = DefaultMaxMappings
	}
	if c.MaxMappings > 5_000_000 {
		return errors.New("redaction: max_mappings is too large")
	}
	if c.MaxTokenLength <= 0 {
		c.MaxTokenLength = DefaultMaxTokenLength
	}
	if c.MaxTokenLength > 1_000_000 {
		return errors.New("redaction: max_token_length is too large")
	}
	if c.MaxRestoreBuffer <= 0 {
		c.MaxRestoreBuffer = DefaultMaxRestoreBuffer
	}
	if c.MaxRestoreBuffer > 512<<20 {
		return errors.New("redaction: max_restore_buffer is too large")
	}
	if c.TokenTTLSeconds <= 0 {
		c.TokenTTLSeconds = DefaultTokenTTL
	}
	c.FailClosedValue = true
	if c.FailClosed != nil {
		c.FailClosedValue = *c.FailClosed
	}
	if c.ToolGuard != nil {
		c.ToolGuard.normalize()
		if c.ToolGuard.Enabled && c.ToolGuard.Mode != ToolGuardObserve && c.ToolGuard.Mode != ToolGuardRewrite && c.ToolGuard.Mode != ToolGuardBlock {
			return errors.New("redaction: tool_guard.mode must be observe, rewrite or block")
		}
	}
	for _, label := range c.DisabledRules {
		if !knownBuiltinLabel(label) {
			return fmt.Errorf("redaction: unknown disabled rule label %q", label)
		}
	}
	for label, words := range c.CustomWords {
		if !validLabel(label) || label == numericCredentialLabel {
			return fmt.Errorf("redaction: invalid custom word label %q", label)
		}
		if len(words) > 4096 {
			return fmt.Errorf("redaction: custom label %q has too many words", label)
		}
		for _, w := range words {
			if len(w) == 0 || len(w) > 8192 || strings.ContainsAny(w, "\r\n\x00") {
				return fmt.Errorf("redaction: invalid custom word under label %q", label)
			}
			if strings.HasPrefix(w, "re:") {
				if _, err := regexp.Compile(strings.TrimPrefix(w, "re:")); err != nil {
					return fmt.Errorf("redaction: invalid regex under label %q: %v", label, err)
				}
			}
		}
	}
	return nil
}

func knownBuiltinLabel(label string) bool {
	for _, r := range builtinRules() {
		if r.label == label {
			return true
		}
	}
	return false
}

// validLabel restricts placeholder labels to the placeholder grammar so
// tokens remain unambiguous and restorable.
func validLabel(label string) bool {
	if len(label) == 0 || len(label) > 32 {
		return false
	}
	for _, b := range label {
		if !(b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_') {
			return false
		}
	}
	return true
}
