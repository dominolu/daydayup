package redact

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Credential labels are classified independently of values. Explicit fields
// and assignments can contain short, alphabetic, Unicode or quoted passwords;
// they must not depend on a provider prefix, entropy or a 64-character limit.
type credentialLabel struct {
	known, weak, authorization bool
}

var credentialAliases = strings.Fields(`password passwd pwd passphrase loginpassword databasepassword dbpassword userpassword rootpassword
	secret clientsecret appsecret apisecret signingsecret secretkey secretaccesskey privatekey authkey appkey clientkey signingkey encryptionkey
	token bearertoken accesstoken refreshtoken idtoken authtoken authenticationtoken securitytoken sessiontoken csrftoken xsrftoken personaltoken personalaccesstoken
	apikey accesskey accesskeyid apitoken xapikey xauthtoken xaccesstoken xtoken xcsrftoken xxsrftoken sessionid sid
	密码 密碼 密码值 密碼值 口令 密钥 密鑰 密钥值 密鑰值 秘钥 秘鑰 密匙 金钥 金鑰 私钥 私鑰 凭据 凭证 憑據 憑證 授权码 授權碼 令牌 令牌值 访问密钥 訪問密鑰 接口密钥 访问令牌 訪問令牌 刷新令牌 安全令牌`)

var credentialAliasSet = func() map[string]bool {
	set := make(map[string]bool, len(credentialAliases))
	for _, name := range credentialAliases {
		set[name] = true
	}
	return set
}()

var credentialWordRE = func() *regexp.Regexp {
	var chinese []string
	for _, alias := range credentialAliases {
		if r, _ := utf8.DecodeRuneInString(alias); r > unicode.MaxASCII {
			chinese = append(chinese, alias)
		}
	}
	sort.SliceStable(chinese, func(i, j int) bool { return len(chinese[i]) > len(chinese[j]) })
	return regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.-]*(?:` + names(chinese...) + `)?|` + names(chinese...))
}()

func normalizedCredentialName(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
}

func classifyCredentialLabel(name string) credentialLabel {
	// Bound suffix classification work on arbitrary source-code identifiers.
	if len(name) > 256 || strings.HasSuffix(name, "-") || strings.HasSuffix(name, ".") {
		return credentialLabel{}
	}
	normalized := normalizedCredentialName(name)
	if normalized == "authorization" || normalized == "proxyauthorization" {
		return credentialLabel{known: true, authorization: true}
	}
	if normalized == "key" || normalized == "pass" {
		return credentialLabel{known: true, weak: true}
	}
	if credentialAliasSet[normalized] {
		return credentialLabel{known: true, authorization: strings.HasSuffix(normalized, "token")}
	}
	// Bilingual labels such as “密码 password” or “Token 令牌” are one label.
	if words := strings.Fields(name); len(words) > 1 {
		combined := credentialLabel{known: true}
		for _, word := range words {
			label := classifyCredentialLabel(word)
			if !label.known {
				combined.known = false
				break
			}
			combined.authorization = combined.authorization || label.authorization
		}
		if combined.known {
			return combined
		}
	}
	for _, alias := range credentialAliases {
		if r, _ := utf8.DecodeRuneInString(alias); r > unicode.MaxASCII && strings.HasSuffix(name, alias) {
			return credentialLabel{known: true, authorization: strings.HasSuffix(alias, "令牌")}
		}
	}
	for _, suffix := range []string{"_value", "-value", ".value", "Value", "值"} {
		if strings.HasSuffix(name, suffix) {
			return classifyCredentialLabel(strings.TrimSuffix(name, suffix))
		}
	}
	// Environment variables and dotted/camelCase configuration names commonly
	// prefix a credential label. Only strong suffixes qualify; primary_key,
	// public_key, max_tokens and password_length remain ordinary data.
	for i, r := range name {
		if i == 0 || !(r == '_' || r == '-' || r == '.' || r >= 'A' && r <= 'Z') {
			continue
		}
		start := i
		if r == '_' || r == '-' || r == '.' {
			start++
		}
		suffix := normalizedCredentialName(name[start:])
		if credentialAliasSet[suffix] && suffix != "sid" && suffix != "sessionid" {
			return credentialLabel{known: true, authorization: strings.HasSuffix(suffix, "token")}
		}
	}
	return credentialLabel{}
}

func skipCredentialSpace(text string, pos int) int {
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !unicode.IsSpace(r) {
			break
		}
		pos += size
	}
	return pos
}

func credentialSeparator(text string, pos int) (int, bool, bool) {
	// Labels may be quoted JSON/YAML names or emphasized Markdown labels.
	for pos < len(text) {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if !strings.ContainsRune(`"'”’」』*`+"`", r) {
			break
		}
		pos += size
	}
	start := pos
	pos = skipCredentialSpace(text, pos)
	spaced := pos != start
	natural := false
	for _, word := range []string{"设置为", "設置為", "为", "為", "是", "is"} {
		if len(text)-pos < len(word) || !strings.EqualFold(text[pos:pos+len(word)], word) {
			continue
		}
		end := pos + len(word)
		if word == "is" && end < len(text) && isCredentialIdentifier(text[end]) {
			continue
		}
		pos = skipCredentialSpace(text, end)
		natural = true
		break
	}
	for _, separator := range []string{"->", "=>", ":=", ":", "=", "：", "＝", "→"} {
		if strings.HasPrefix(text[pos:], separator) {
			return skipCredentialSpace(text, pos+len(separator)), true, true
		}
	}
	return pos, false, spaced || natural
}

func isCredentialIdentifier(b byte) bool {
	return isAlnum(b) || b == '_' || b == '-' || b == '.'
}

func credentialValue(text string, start int) (int, int, bool) {
	if start >= len(text) {
		return start, start, false
	}
	opener, size := utf8.DecodeRuneInString(text[start:])
	closer := map[rune]rune{'"': '"', '\'': '\'', '`': '`', '“': '”', '‘': '’', '「': '」', '『': '』'}[opener]
	if closer != 0 {
		begin := start + size
		for end := begin; end < len(text); {
			r, n := utf8.DecodeRuneInString(text[end:])
			if r == '\\' && end+n < len(text) {
				_, escaped := utf8.DecodeRuneInString(text[end+n:])
				end += n + escaped
				continue
			}
			if r == closer {
				return begin, end, true
			}
			end += n
		}
		// An unfinished quote must not cause the rest of the credential to leak.
		return begin, len(text), true
	}
	end := start
	for end < len(text) {
		r, n := utf8.DecodeRuneInString(text[end:])
		if unicode.IsSpace(r) || strings.ContainsRune(",;，；。&<>\"'”’」』)]}"+"`", r) {
			break
		}
		if r == '\\' && end+n < len(text) {
			_, escaped := utf8.DecodeRuneInString(text[end+n:])
			end += n + escaped
		} else {
			end += n
		}
	}
	return start, end, false
}

func credentialExplanation(value string) bool {
	switch strings.ToLower(value) {
	case "should", "must", "requires", "required", "require", "needs", "need", "can", "cannot", "will", "would", "could", "may", "is", "are", "was", "the", "a", "an", "your", "my", "this", "that", "has", "have", "be", "not", "please", "reset", "change", "length", "strength", "policy", "field", "type", "description", "format", "budget", "count", "limit", "usage", "authentication", "authorization", "scheme", "benefit", "enter", "space", "escape", "arrowup", "arrowdown":
		return true
	}
	for _, prefix := range []string{"需要", "应该", "必须", "可以", "长度", "说明", "格式", "不能为空", "為必填", "为必填"} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func credentialValueAllowed(value string, label credentialLabel, explicit, quoted bool) bool {
	if value == "" {
		return false
	}
	if explicit || quoted {
		return true
	}
	if credentialExplanation(value) {
		return false
	}
	if !label.weak || len(value) >= 20 {
		return true
	}
	return strings.ContainsAny(value, "0123456789!@#$%^&*/+=_-")
}

func authorizationValueStart(text string, start int) int {
	for _, scheme := range []string{"bearer", "basic", "token", "apikey"} {
		end := start + len(scheme)
		if end < len(text) && strings.EqualFold(text[start:end], scheme) && skipCredentialSpace(text, end) > end {
			return skipCredentialSpace(text, end)
		}
	}
	return start
}

func unquotedCredentialEnd(text string, start int, closer rune) int {
	end := start
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		if unicode.IsSpace(r) || closer != 0 && r == closer {
			break
		}
		// A punctuation separator followed by whitespace ends an assignment.
		// Inside a word (or at its end), punctuation can be part of a password.
		if strings.ContainsRune(",;，；。&)]}", r) && end+size < len(text) && skipCredentialSpace(text, end+size) > end+size {
			break
		}
		if r == '\\' && end+size < len(text) {
			_, escaped := utf8.DecodeRuneInString(text[end+size:])
			end += size + escaped
		} else {
			end += size
		}
	}
	return end
}

func credentialSpans(value, field string) [][2]int {
	if label := classifyCredentialLabel(field); label.known && value != "" {
		start := 0
		if label.authorization {
			start = authorizationValueStart(value, skipCredentialSpace(value, 0))
		}
		return [][2]int{{start, len(value)}}
	}
	var spans [][2]int
	words := credentialWordRE.FindAllStringIndex(value, -1)
	coveredUntil := 0
	for i, word := range words {
		flagStart := word[0]
		for flagStart > 0 && value[flagStart-1] == '-' {
			flagStart--
		}
		beforeFlag, _ := utf8.DecodeLastRuneInString(value[:flagStart])
		cliFlag := word[0] > flagStart && (flagStart == 0 || unicode.IsSpace(beforeFlag) || strings.ContainsRune("`\"'([", beforeFlag))
		if word[0] < coveredUntil || word[0] > 0 && !cliFlag && (isCredentialIdentifier(value[word[0]-1]) || strings.ContainsRune("?&", rune(value[word[0]-1]))) {
			continue
		}
		last := i
		for j := i + 1; j < len(words) && j < i+4; j++ {
			if skipCredentialSpace(value, words[j-1][1]) != words[j][0] || words[j-1][1] == words[j][0] {
				break
			}
			last = j
		}
		for j := last; j >= i; j-- {
			label := classifyCredentialLabel(value[word[0]:words[j][1]])
			if !label.known {
				continue
			}
			start, explicit, ok := credentialSeparator(value, words[j][1])
			if !ok {
				continue
			}
			begin, end, quoted := credentialValue(value, start)
			if label.authorization {
				if quoted {
					begin = authorizationValueStart(value[:end], begin)
				} else {
					begin, end, quoted = credentialValue(value, authorizationValueStart(value, start))
				}
			}
			if !quoted {
				var closer rune
				labelStart := word[0]
				if cliFlag {
					labelStart = flagStart
				}
				if labelStart > 0 {
					before, _ := utf8.DecodeLastRuneInString(value[:labelStart])
					if strings.ContainsRune("\"'`", before) && (words[j][1] == len(value) || rune(value[words[j][1]]) != before) {
						closer = before
					}
				}
				end = unquotedCredentialEnd(value, begin, closer)
			}
			if credentialValueAllowed(value[begin:end], label, explicit, quoted) {
				spans = append(spans, [2]int{begin, end})
				coveredUntil = end
				break
			}
		}
	}
	return spans
}

var queryCredentialRE = regexp.MustCompile(`[?&]([^?&#\s"'<>\\=]+)=`)

func queryCredentialSpans(value, _ string) [][2]int {
	var spans [][2]int
	for _, match := range queryCredentialRE.FindAllStringSubmatchIndex(value, -1) {
		name, err := url.QueryUnescape(value[match[2]:match[3]])
		if err != nil || !classifyCredentialLabel(name).known && !strings.HasSuffix(strings.ToLower(name), "signature") && !strings.EqualFold(name, "sig") {
			continue
		}
		end := match[1]
		for end < len(value) && !strings.ContainsRune("&# \t\r\n\"'<>\\{}[]", rune(value[end])) {
			end++
		}
		if end > match[1] {
			spans = append(spans, [2]int{match[1], end})
		}
	}
	return spans
}

var bearerCredentialRE = regexp.MustCompile(`\b[Bb][Ee][Aa][Rr][Ee][Rr][ \t]+`)

func bearerCredentialSpans(value, _ string) [][2]int {
	var spans [][2]int
	for _, m := range bearerCredentialRE.FindAllStringIndex(value, -1) {
		begin, end, _ := credentialValue(value, m[1])
		candidate := strings.ToLower(value[begin:end])
		if candidate == "token" || strings.HasPrefix(candidate, "token:") || strings.HasPrefix(candidate, "token=") || strings.HasPrefix(candidate, "token：") {
			continue
		}
		if end > begin && !credentialExplanation(value[begin:end]) {
			spans = append(spans, [2]int{begin, end})
		}
	}
	return spans
}
