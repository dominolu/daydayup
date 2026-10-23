// Package workbuddy adapts Tencent WorkBuddy (www.workbuddy.ai, international)
// and CodeBuddy (copilot.tencent.com / www.codebuddy.cn, China) native chat
// service accounts into the gateway Provider interface. The upstream chat
// endpoint is OpenAI-compatible (/v2/chat/completions), so requests pass
// through with identity headers and a path rewrite instead of translation.
package workbuddy

import (
	"bytes"
	"net/http"

	"safuno/internal/config"
	wbp "safuno/internal/provider"
)

type WorkBuddy struct{}

var _ wbp.Provider = WorkBuddy{}
var _ wbp.SessionAuthorizer = WorkBuddy{}
var _ wbp.PathRewriter = WorkBuddy{}
var _ wbp.Refreshable = WorkBuddy{}
var _ wbp.QuotaExhausted = WorkBuddy{}
var _ wbp.Restriction = WorkBuddy{}
var _ wbp.BodyTransformer = WorkBuddy{}

func (WorkBuddy) Name() string { return "workbuddy" }

// chatPath is the upstream chat endpoint the desktop/CLI/VSCode identities
// all speak (OpenAI Chat Completions wire format).
const chatPath = "/v2/chat/completions"

// RewritePath maps the gateway's /v1/* surface to the WorkBuddy upstream
// paths. Only chat completions are currently native; everything else returns
// ok=false so the gateway forwards the client path verbatim.
func (WorkBuddy) RewritePath(clientPath string) (string, bool) {
	if clientPath == "/v1/chat/completions" {
		return chatPath, true
	}
	return clientPath, false
}

// Authorize is the stateless fallback (no resolved session context).
func (w WorkBuddy) Authorize(r *http.Request, a config.Account) {
	w.AuthorizeSession(r, a, "", "")
}

// AuthorizeSession injects the WorkBuddy identity header family, including
// session-derived conversation ids once the gateway has resolved a session,
// and the VSCode identity's model headers (X-Model-ID + monitor timestamps).
func (WorkBuddy) AuthorizeSession(r *http.Request, a config.Account, sessionKey, model string) {
	r.Header.Set("Authorization", "Bearer "+a.APIKey)
	r.Header.Set("X-User-Id", uidOf(a))
	// Only the chat route carries the full client identity. Other /v1/*
	// passthroughs get credentials only and keep whatever headers the client
	// sent; faking a chat identity on unrelated routes is a gateway trait.
	if r.URL.Path != chatPath {
		return
	}
	for k, v := range identityHeaders(a, sessionKey, model) {
		r.Header.Set(k, v)
	}
}

func uidOf(a config.Account) string {
	if a.UID != "" {
		return a.UID
	}
	return "anonymous"
}

// ModelRateLimit reports a model-scoped upstream cap (429 / code 6004), the
// only rate-limit answer the upstream scopes to the selected model.
func (WorkBuddy) ModelRateLimit(body []byte) bool {
	return bytes.Contains(body, []byte("6004"))
}

// ModelUnavailable reports the account-level abuse restriction (403 / code
// 11140); the gateway denies the model on that account and Restriction()
// additionally cools the whole realm so other accounts are not implicated.
func (WorkBuddy) ModelUnavailable(body []byte) bool {
	return bytes.Contains(body, []byte("11140"))
}

// QuotaExhausted reports the definitive out-of-credits answer (HTTP 402).
func (WorkBuddy) QuotaExhausted(body []byte) bool {
	return len(bytes.TrimSpace(body)) > 0
}

// Restriction reports a 403 / code 11140 account restriction.
func (WorkBuddy) Restriction(body []byte) bool {
	return bytes.Contains(body, []byte("11140"))
}
