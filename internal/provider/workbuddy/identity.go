package workbuddy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"time"

	"safuno/internal/config"
)

// realmConfig holds the per-realm upstream endpoints and client identities.
// Values are aligned with the official WorkBuddy desktop client 5.8.4
// (international) capture and the documented CN desktop values; see
// workbuddy2api-hub wb_identity.py / wb_accounts.py for the provenance.
type realmConfig struct {
	name            string
	chatUpstream    string
	billingUpstream string
	origin          string
	domain          string
	chatUA          string
	billingUA       string
	acceptLang      string
	ideVersion      string
}

var realms = map[string]realmConfig{
	"intl": {
		name:            "国际版 (Global)",
		chatUpstream:    "https://www.workbuddy.ai",
		billingUpstream: "https://www.workbuddy.ai",
		origin:          "https://www.workbuddy.ai",
		domain:          "www.workbuddy.ai",
		chatUA:          "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0",
		billingUA:       "WorkBuddy/5.8.4",
		acceptLang:      "en-US",
		ideVersion:      "5.8.4",
	},
	"cn": {
		name:            "国内版 (China)",
		chatUpstream:    "https://copilot.tencent.com",
		billingUpstream: "https://www.codebuddy.cn",
		origin:          "https://www.codebuddy.cn",
		domain:          "copilot.tencent.com",
		chatUA:          "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1",
		billingUA:       "WorkBuddy/5.5.6",
		acceptLang:      "zh-CN",
		ideVersion:      "5.5.6",
	},
}

func realmOf(a config.Account) realmConfig {
	return realms[config.RealmOf(&a)]
}

func (r realmConfig) domainFor(a config.Account) string {
	if d := strings.TrimSpace(a.Domain); d != "" {
		return d
	}
	// A custom origin (self-hosted mirror, VSCode CN on www.workbuddy.cn)
	// advertises its own host as X-Domain so the header matches where the
	// request is actually sent.
	if u, err := url.Parse(a.BaseURL); err == nil && u.Host != "" && !strings.EqualFold(u.Host, r.domain) {
		return u.Host
	}
	return r.domain
}

// ---- ID derivation (device/session fingerprint alignment) ----

// deriveID derives a stable UUID-shaped identifier from (uid, salt): every
// account always presents as the same virtual device, and distinct accounts
// are never correlated through a shared machine id.
func deriveID(uid, salt string) string {
	h := sha256.Sum256([]byte(salt + ":" + uid))
	b := h[:16]
	// Force UUIDv4 shape (version 4, variant 10) exactly like real clients.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// sessionIDFor is the account-scoped X-Session-ID used on billing requests.
func sessionIDFor(uid string) string { return deriveID(uid, "session") }

// machineIDFor is the account-scoped X-Machine-ID used on billing requests.
func machineIDFor(uid string) string { return deriveID(uid, "machine") }

// conversationIDFor derives a stable conversation id for (uid, session key).
// The official client reuses the same X-Conversation-ID across one chat; a
// per-account derivation keeps multi-turn chats looking human without ever
// correlating two accounts through a shared conversation id. Empty session
// key returns "" so callers fall back to a fresh per-request id.
func conversationIDFor(uid, sessionKey string) string {
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return ""
	}
	h := sha256.Sum256([]byte(uid + "|" + key))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// uuid7 returns an RFC 9562 UUIDv7 (48-bit millisecond timestamp first, then
// random bits): the request/message id shape of the 5.8.4 client (32-hex
// without dashes after formatting).
func uuid7() [16]byte {
	ms := uint64(time.Now().UnixMilli()) & ((1 << 48) - 1)
	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	rest := make([]byte, 10)
	if _, err := rand.Read(rest); err != nil {
		// rand failure is practically impossible; fall back to zero bits.
		return b
	}
	b[6] = (rest[0] & 0x0f) | 0x70 // version 7
	b[7] = rest[1]
	b[8] = (rest[2] & 0x3f) | 0x80 // variant 10
	for i := 9; i < 16; i++ {
		b[i] = rest[i-6]
	}
	return b
}

// newMessageID returns the 32-hex request/message id shape.
func newMessageID() string {
	u := uuid7()
	return hex.EncodeToString(u[:])
}

// newConversationID returns a dashed UUID conversation id.
func newConversationID() string {
	u := uuid7()
	s := hex.EncodeToString(u[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// traceHeaders is the link header family the desktop chat request carries.
func traceHeaders(withParent bool) map[string]string {
	trace := newMessageID()
	span := randomHex(8)
	h := map[string]string{
		"X-Trace-ID":   trace,
		"traceparent":  "00-" + trace + "-" + span + "-01",
		"b3":           trace + "-" + span + "-1",
		"X-B3-TraceId": trace,
		"X-B3-SpanId":  span,
		"X-B3-Sampled": "1",
	}
	if withParent {
		h["X-B3-ParentSpanId"] = randomHex(8)
	}
	return h
}

// vscodeTraceHeaders is the B3 header family of the official VSCode extension.
func vscodeTraceHeaders() map[string]string {
	trace := newMessageID()
	span := randomHex(8)
	parent := randomHex(8)
	return map[string]string{
		"X-Trace-ID":        trace,
		"b3":                trace + "-" + span + "-1-" + parent,
		"X-B3-TraceId":      trace,
		"X-B3-ParentSpanId": parent,
		"X-B3-SpanId":       span,
		"X-B3-Sampled":      "1",
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
