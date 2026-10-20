package redact

import (
	"regexp"
	"sort"
	"strings"
)

// rule is one masking rule. Patterns are run over the request text; the value
// span (whole match or a capture group) is replaced by a stable placeholder.
type rule struct {
	label        string
	patterns     []*regexp.Regexp
	captureGroup int
	semantic     bool
	findSpans    func(value, field string) [][2]int
}

// builtinRules returns the ordered rule table. Value rules run first so a credential
// match wins over the PII that may be embedded inside it.
func builtinRules() []*rule {
	return []*rule{
		{label: "PRIVATE_KEY", patterns: []*regexp.Regexp{re(`-----BEGIN (RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----[\s\S]{20,}?-----END [^-]*PRIVATE KEY-----`)}},
		{label: "CONNSTR", patterns: []*regexp.Regexp{re(`[a-z][a-z0-9+.-]{0,63}://[^\s:@/]+:([^\s@/]+)@`)}, captureGroup: 1},
		{label: "BASIC_AUTH", patterns: []*regexp.Regexp{re(`\b` + names(`authorization`, `proxy-authorization`) + `["']?[ \t]*[:=][ \t]*["']?[Bb][Aa][Ss][Ii][Cc][ \t]+([A-Za-z0-9+/]+={0,2})(?:$|[ \t\r\n"'\\,;}])`)}, captureGroup: 1},
		{label: "COOKIE_SESSION", patterns: []*regexp.Regexp{re(`(?:^|[^A-Za-z0-9_.?&-])` + names("session", "sessionid", "session_id", "session-id", "sid", "jsessionid", "phpsessid", "asp.net_sessionid", "connect.sid", "next-auth.session-token", "authjs.session-token", "__secure-next-auth.session-token", "__host-next-auth.session-token", "__secure-authjs.session-token", "__host-authjs.session-token", "__host-session", "__secure-session") + `["']?[ \t]*[:=][ \t]*["']?([^;\s"'\\,{}\[\]]{8,})`)}, captureGroup: 1},
		{label: "URL_QUERY_SECRET", findSpans: queryCredentialSpans},
		{label: "API_KEY", patterns: []*regexp.Regexp{
			re(`sk-[A-Za-z0-9_-]{16,}`),
			re(`(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{20,}`),
			re(`github_pat_[A-Za-z0-9_]{50,}`),
			re(`AIza[0-9A-Za-z_-]{35,38}`),
			re(`xox[baprs]-[0-9A-Za-z-]{10,}`),
			re(`[sr]k_(live|test)_[0-9A-Za-z]{20,}`),
			re(`cli_[a-z0-9]{16,}`),
			re(`ding[a-z0-9]{6,}`),
		}},
		{label: "ACCESS_KEY", patterns: []*regexp.Regexp{
			re(`(AKIA|ASIA)[A-Z0-9]{16}`),
			re(`LTAI[A-Za-z0-9]{12,20}`),
			re(`AKID[A-Za-z0-9]{13,32}`),
		}},
		{label: "AWS_SECRET", patterns: []*regexp.Regexp{re(`(aws_secret_access_key|AWS_SECRET_ACCESS_KEY)["']?\s*[:=]\s*["']?([A-Za-z0-9/+=]{40})`)}, captureGroup: 2},
		{label: "JWT", patterns: []*regexp.Regexp{re(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)}, semantic: true},
		{label: "TOKEN", findSpans: bearerCredentialSpans},
		{label: "SECRET", findSpans: credentialSpans},
		{label: "PHONE", patterns: []*regexp.Regexp{
			re(`1[3-9][0-9]{9}`),
			re(`1[3-9][0-9][ -][0-9]{4}[ -][0-9]{4}`),
			re(`(\+?86|0086|\(\+?86\)|（\+?86）)[ -]?1[3-9][0-9][ -]?[0-9]{4}[ -]?[0-9]{4}`),
		}, semantic: true},
		{label: "LANDLINE", patterns: []*regexp.Regexp{re(`0(10|2[0-9]|[3-9][0-9]{2})[- ]?[2-9][0-9]{6,7}`)}, semantic: true},
		{label: "EMAIL", patterns: []*regexp.Regexp{re(`([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,})`)}, semantic: true},
		{label: "IDCARD", patterns: []*regexp.Regexp{re(`((1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{15}[0-9Xx]|(1[1-5]|2[1-3]|3[1-7]|4[1-6]|5[0-4]|6[1-5]|71|8[12])[0-9]{13})`)}, semantic: true},
		{label: "CARD", patterns: []*regexp.Regexp{re(`[3-6][0-9]{12,18}`)}, semantic: true},
		{label: "IBAN", patterns: []*regexp.Regexp{re(`[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}`)}, semantic: true},
		{label: "USCC", patterns: []*regexp.Regexp{re(`[0-9A-HJ-NPQRTUWXY]{2}[0-9]{6}[0-9A-HJ-NPQRTUWXY]{10}`)}, semantic: true},
		{label: "MAC", patterns: []*regexp.Regexp{re(`[0-9A-Fa-f]{2}[:-][0-9A-Fa-f]{2}[:-][0-9A-Fa-f]{2}[:-][0-9A-Fa-f]{2}[:-][0-9A-Fa-f]{2}[:-][0-9A-Fa-f]{2}`)}},
		{label: "PLATE", patterns: []*regexp.Regexp{re(`(京|津|沪|渝|冀|豫|云|辽|黑|湘|皖|鲁|新|苏|浙|赣|鄂|桂|甘|晋|蒙|陕|吉|闽|贵|粤|青|藏|川|宁|琼|使|领)[A-Z][A-Z0-9]{5,6}`)}, semantic: true},
		{label: "HKID", patterns: []*regexp.Regexp{re(`H[0-9]{8}`)}},
		{label: "IP_PRIVATE", patterns: []*regexp.Regexp{re(`(192\.168|169\.254|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7]))\.[0-9]{1,3}\.[0-9]{1,3}`)}, semantic: true},
		{label: "IP_INTERNAL", patterns: []*regexp.Regexp{re(`(10\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01]))\.[0-9]{1,3}\.[0-9]{1,3}`)}, semantic: true},
		{label: "IP_PUBLIC", patterns: []*regexp.Regexp{re(`([0-9]\.|[0-9][0-9]\.|[0-9][0-9][0-9]\.){3}[0-9]{1,3}`)}, semantic: true},
		{label: "IPV6_PRIVATE", patterns: []*regexp.Regexp{re(`(fe[89AaBb]|fc|fd)[0-9A-Fa-f:]{1,44}`)}, semantic: true},
	}
}

// names folds each ASCII word into a case-insensitive alternation.
func names(values ...string) string {
	var b strings.Builder
	b.WriteString("(?:")
	for i, v := range values {
		if i > 0 {
			b.WriteByte('|')
		}
		for _, r := range v {
			if r >= 'a' && r <= 'z' {
				b.WriteByte('[')
				b.WriteRune(r)
				b.WriteRune(r - 'a' + 'A')
				b.WriteByte(']')
			} else {
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
	}
	b.WriteString(")")
	return b.String()
}

func re(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// builtinDescriptions provides the display names used by the management API.
func builtinDescriptions() map[string]string { return BuiltinDescriptions() }

// BuiltinDescriptions provides the display names used by the management API.
func BuiltinDescriptions() map[string]string {
	return map[string]string{
		"PRIVATE_KEY": "PEM 私钥（整块替换）", "CONNSTR": "连接串密码（只脱密码）",
		"PHONE": "手机号", "EMAIL": "邮箱", "LANDLINE": "座机号码",
		"PLATE": "车牌", "HKID": "港澳通行证", "IDCARD": "身份证",
		"IP_PRIVATE": "私网 IPv4", "IP_INTERNAL": "内网 IPv4", "IP_PUBLIC": "公网 IPv4",
		"IPV6_PRIVATE": "私网 IPv6", "CARD": "银行卡号", "IBAN": "IBAN",
		"USCC": "统一社会信用代码", "MAC": "MAC 地址", "API_KEY": "常见 API Key",
		"ACCESS_KEY": "云厂商访问密钥", "AWS_SECRET": "AWS Secret 赋值", "JWT": "JWT", "TOKEN": "Bearer Token", "SECRET": "凭据上下文与赋值（只脱值）",
		"COOKIE_SESSION":   "常见会话字段与登录 Cookie（只脱值）",
		"BASIC_AUTH":       "HTTP Basic 认证载荷（只脱值）",
		"URL_QUERY_SECRET": "URL 查询参数中的凭据与签名（只脱值）",
	}
}

// BuiltinCatalog lists the builtin rule labels in stable order.
func BuiltinCatalog() []string {
	out := make([]string, 0, len(BuiltinDescriptions()))
	for label := range BuiltinDescriptions() {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}
