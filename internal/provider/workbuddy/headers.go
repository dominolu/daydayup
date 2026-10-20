package workbuddy

import (
	"runtime"
	"strconv"
	"time"

	"daydayup/internal/config"
)

// identityHeaders builds the full header family for the account product
// (desktop by default; vscode/cli opt-in identities for P4). model only takes
// effect on the VSCode identity, which carries X-Model-ID plus the two
// monitor timestamps on model requests (official extension behavior).
func identityHeaders(a config.Account, sessionKey, model string) map[string]string {
	rc := realmOf(a)
	p := config.DefaultProduct(a)
	uid := uidOf(a)

	var conv string
	if c := conversationIDFor(uid, sessionKey); c != "" {
		conv = c
	} else {
		conv = newConversationID()
	}
	convReqID := newMessageID()
	msgID := newMessageID()

	h := map[string]string{}
	switch p {
	case config.ProductVSCode:
		h["X-Agent-Intent"] = "craft"
		h["X-Agent-Type"] = "main"
		h["X-IDE-Type"] = "VSCode"
		h["X-IDE-Name"] = "VSCode"
		h["X-IDE-Version"] = "1.119.0"
		h["X-Product-Version"] = "4.12.39997873"
		h["X-Env-ID"] = "production"
		h["X-Product"] = "SaaS"
		h["X-Domain"] = rc.domainFor(a)
		h["User-Agent"] = "VSCode/1.119.0 CodeBuddy/4.12.39997873"
		h["X-Request-Trace-Id"] = newMessageID()
		h["X-Conversation-ID"] = conv
		h["X-Conversation-Request-ID"] = convReqID
		h["X-Conversation-Message-ID"] = msgID
		h["X-Request-ID"] = convReqID
		if !isCN(a) {
			h["X-Private-Data"] = "true"
		}
		if a.EnterpriseID == "" {
			// Official build always sends a device token or its error state.
			h["X-Device-Token-Error"] = "sdk_not_initialized"
		}
		if model != "" {
			// Official BaseAgent stamps the requested model id and two
			// millisecond monitor timestamps on model requests.
			nowMS := strconv.FormatInt(time.Now().UnixMilli(), 10)
			h["X-Model-ID"] = model
			h["monitor_promptPrepareStartTime"] = nowMS
			h["monitor_httpSendTime"] = nowMS
		}
		for k, v := range vscodeTraceHeaders() {
			h[k] = v
		}
	case config.ProductCLI:
		// Official CodeBuddy CLI 2.168.2 identity (OpenAI Node SDK 6.25.0
		// headers + CLI product constants), per wb_identity.py.
		h["X-Agent-Intent"] = "craft"
		h["X-Agent-Purpose"] = "conversation"
		h["X-Agent-Runtime"] = "other"
		h["X-Agent-Type"] = "main"
		h["X-IDE-Type"] = "CLI"
		h["X-IDE-Name"] = "CLI"
		h["X-IDE-Version"] = cliVersion
		h["X-Product"] = "SaaS"
		h["X-Domain"] = rc.domainFor(a)
		h["User-Agent"] = "CLI/" + cliVersion + " CodeBuddy/" + cliVersion
		h["X-Conversation-ID"] = conv
		h["X-Conversation-Request-ID"] = convReqID
		h["X-Root-Request-ID"] = convReqID
		h["X-Conversation-Message-ID"] = msgID
		h["X-Request-ID"] = msgID
		// Official getCustomHeaders: personal accounts default false, enterprise true.
		if a.EnterpriseID != "" {
			h["X-Private-Data"] = "true"
		} else {
			h["X-Private-Data"] = "false"
		}
		for k, v := range stainlessHeaders() {
			h[k] = v
		}
		for k, v := range traceHeaders(true) {
			h[k] = v
		}
	default: // desktop
		h["X-Agent-Intent"] = "craft"
		h["X-Agent-Purpose"] = "conversation"
		h["X-Agent-Type"] = "main"
		h["X-IDE-Name"] = "WorkBuddy"
		h["X-IDE-Type"] = "WorkBuddy"
		h["X-IDE-Version"] = rc.ideVersion
		h["X-Private-Data"] = "true"
		h["X-Product"] = "SaaS"
		h["X-Domain"] = rc.domainFor(a)
		h["User-Agent"] = rc.chatUA
		h["X-Conversation-ID"] = conv
		h["X-Conversation-Request-ID"] = convReqID
		h["X-Root-Request-ID"] = convReqID
		h["X-Conversation-Message-ID"] = msgID
		h["X-Request-ID"] = msgID
		for k, v := range traceHeaders(false) {
			h[k] = v
		}
	}
	// Browser-ish common headers (desktop and vscode identities only).
	if p != config.ProductCLI {
		h["Accept"] = "application/json, text/plain, */*"
		h["X-Requested-With"] = "XMLHttpRequest"
		h["Origin"] = rc.origin
		h["Referer"] = rc.origin + "/"
		h["X-CodeBuddy-Request"] = "1"
		h["Accept-Language"] = rc.acceptLang
	} else {
		h["Accept"] = "application/json"
	}
	return h
}

func isCN(a config.Account) bool { return config.RealmOf(&a) == "cn" }

// cliVersion is the official CodeBuddy CLI package version (npm
// @tencent-ai/codebuddy-code), used for UA and X-IDE-Version.
const cliVersion = "2.168.2"

// stainlessHeaders is the OpenAI Node SDK 6.25.0 fingerprint header family the
// official CLI chat requests carry.
func stainlessHeaders() map[string]string {
	return map[string]string{
		"x-stainless-lang":            "js",
		"x-stainless-package-version": "6.25.0",
		"x-stainless-os":              cliOS(),
		"x-stainless-arch":            cliArch(),
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": "v22.21.1",
		"x-stainless-retry-count":     "0",
		"x-stainless-timeout":         "1200",
	}
}

func cliOS() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "MacOS"
	default:
		return "Linux"
	}
}

func cliArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	case "arm64":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

// billingHeaders builds the lighter header family the billing endpoints use.
func billingHeaders(a config.Account) map[string]string {
	rc := realmOf(a)
	uid := uidOf(a)
	h := map[string]string{
		"Content-Type":        "application/json",
		"Accept":              "application/json, text/plain, */*",
		"X-Requested-With":    "XMLHttpRequest",
		"User-Agent":          rc.billingUA,
		"Origin":              rc.origin,
		"Referer":             rc.origin + "/",
		"Authorization":       "Bearer " + a.APIKey,
		"X-User-Id":           uid,
		"X-Domain":            rc.domainFor(a),
		"X-CodeBuddy-Request": "1",
		"Accept-Language":     rc.acceptLang,
		"X-Request-ID":        newMessageID(),
		"X-Machine-ID":        machineIDFor(uid),
		"X-Session-ID":        sessionIDFor(uid),
	}
	if a.EnterpriseID != "" {
		h["X-Enterprise-Id"] = a.EnterpriseID
		h["X-Tenant-Id"] = a.EnterpriseID
	} else {
		h["X-No-Enterprise-Id"] = "1"
	}
	if isCN(a) {
		h["X-Product"] = "SaaS"
	}
	return h
}
