package redact

import (
	"regexp"
	"sync"
)

// ToolGuard mode observes dangerous tool calls. Stage 2 implements observe;
// rewrite and block (withhold) are reserved for a later stage.
const (
	ToolGuardObserve = "observe"
	ToolGuardRewrite = "rewrite"
	ToolGuardBlock   = "block"
)

// ToolGuard is embedded in the redaction config. In observe mode findings are
// counted and logged but tool calls still reach the client.
type ToolGuard struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"`
}

func (g *ToolGuard) normalize() {
	if !g.Enabled {
		return
	}
	if g.Mode == "" {
		g.Mode = ToolGuardObserve
	}
}

// ToolFinding describes one dangerous pattern match in restored tool args.
type ToolFinding struct {
	Rule  string `json:"rule"`
	Label string `json:"label"`
}

var dangerousPatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"rm-rf", re(`\brm\s+(?:-[a-zA-Z]*r[a-zA-Z]*f|-r\s+-f|-f\s+-r)\b`)},
	{"sudo-rm", re(`\bsudo\s+rm\b`)},
	{"fork-bomb", re(`:\(\)\s*\{\s*[:|]`)},
	{"base64-pipe-sh", re(`base64\s+(?:-d|--decode)\b[^&\n|]*\|\s*(?:ba)?sh\b`)},
	{"curl-pipe-sh", re(`\b(?:curl|wget)\b[^&\n|]*\|\s*(?:ba)?sh\b`)},
	{"eval", re(`\beval\s*(?:\(|\[)`)},
	{"mkfs", re(`\bmkfs(?:\.\w+)?\s`)},
	{"dd-block-device", re(`\bdd\s+if=/dev/(?:sd|hd|nvme)`)},
	{"write-block-device", re(`(?:^|[>|])[^<>\n]*/dev/sd[a-z]\b`)},
	{"chmod-root", re(`\bchmod\s+777\s+/`)},
	{"iex", re(`\b(?:Invoke-Expression|iex)\s*\(`)},
	{"openssl-enc", re(`\bopenssl\s+enc\b`)},
}

// CheckToolArgs runs the dangerous-pattern table over restored tool-call
// arguments. In observe mode it only returns findings; nothing is withheld.
func (e *Engine) CheckToolArgs(args string) []ToolFinding {
	e.state.mu.RLock()
	guard := e.state.toolGuard
	e.state.mu.RUnlock()
	if guard == nil || !guard.Enabled {
		return nil
	}
	var findings []ToolFinding
	for _, p := range dangerousPatterns {
		if p.re.MatchString(args) {
			findings = append(findings, ToolFinding{Rule: p.label, Label: p.label})
		}
	}
	if len(findings) > 0 {
		e.statsMu.Lock()
		e.stats.ToolFindings += uint64(len(findings))
		e.statsMu.Unlock()
	}
	return findings
}

// ── hot rule reload ──────────────────────────────────────────────────────

// rulesMu guards the rule table so a hot reload never races in-flight masks.
type rulesState struct {
	mu        sync.RWMutex
	rules     []*rule
	toolGuard *ToolGuard
}

// Reconfigure rebuilds the rule table and tool-guard policy from a new config.
// The mapping store and placeholders are retained; only rules change.
func (e *Engine) Reconfigure(cfg Config) error {
	if err := cfg.Normalize(); err != nil {
		return err
	}
	state := &rulesState{toolGuard: cfg.ToolGuard}
	if state.toolGuard != nil {
		state.toolGuard.normalize()
	}
	state.rules = buildRulesFor(cfg)
	e.state.mu.Lock()
	e.state.rules = state.rules
	e.state.toolGuard = state.toolGuard
	e.state.mu.Unlock()
	return nil
}

func buildRulesFor(cfg Config) []*rule {
	disabled := map[string]bool{}
	for _, label := range cfg.DisabledRules {
		disabled[label] = true
	}
	var rules []*rule
	for _, r := range builtinRules() {
		if disabled[r.label] {
			continue
		}
		copy := *r
		rules = append(rules, &copy)
	}
	labels := make([]string, 0, len(cfg.CustomWords))
	for label := range cfg.CustomWords {
		labels = append(labels, label)
	}
	sortStrings(labels)
	for _, label := range labels {
		r := &rule{label: label}
		for _, word := range cfg.CustomWords[label] {
			if len(word) == 0 || containsCRLF(word) {
				continue
			}
			if isRegexWord(word) {
				if compiled, err := regexp.Compile(trimRegexPrefix(word)); err == nil {
					r.patterns = append(r.patterns, compiled)
				}
			} else {
				r.patterns = append(r.patterns, regexp.MustCompile(regexp.QuoteMeta(word)))
			}
		}
		if len(r.patterns) > 0 {
			rules = append(rules, r)
		}
	}
	return rules
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

func isRegexWord(word string) bool       { return len(word) > 3 && word[:3] == "re:" }
func trimRegexPrefix(word string) string { return word[3:] }
func containsCRLF(word string) bool {
	for i := 0; i < len(word); i++ {
		if word[i] == '\r' || word[i] == '\n' || word[i] == 0 {
			return true
		}
	}
	return false
}
