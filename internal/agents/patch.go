package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The patchers below edit existing config files surgically: only the keys the
// gateway needs are replaced or appended, everything else (comments, ordering,
// unknown fields) stays byte-for-byte identical. That matches the product
// requirement "原子地改写 Agent 的配置，注释、顺序和格式原样保留".

// maskJSONComments returns a same-length copy of content with // and /* */
// comments replaced by spaces. String contents and escaped quotes are kept
// intact, so the masked copy validates as strict JSON and JSON values can be
// located at the same byte offsets as in the original.
func maskJSONComments(content []byte) []byte {
	out := make([]byte, len(content))
	inString := false
	escaped := false
	i := 0
	for i < len(content) {
		c := content[i]
		out[i] = c
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			i++
			continue
		}
		switch {
		case c == '"':
			inString = true
			i++
		case c == '/' && i+1 < len(content) && content[i+1] == '/':
			for i < len(content) && content[i] != '\n' {
				out[i] = ' '
				i++
			}
		case c == '/' && i+1 < len(content) && content[i+1] == '*':
			out[i] = ' '
			out[i+1] = ' '
			i += 2
			for i+1 < len(content) && !(content[i] == '*' && content[i+1] == '/') {
				out[i] = ' '
				i++
			}
			if i+1 < len(content) {
				out[i], out[i+1] = ' ', ' '
				i += 2
			}
		default:
			i++
		}
	}
	return out
}

// jsonValueRange locates the JSON value of a TOP-LEVEL key of the root object.
// It walks the root object with a json.Decoder (on the comment-masked copy)
// and records the byte offset of each key, so a same-named key inside a nested
// object or a string value can never shadow the field we patch.
func jsonValueRange(content []byte, keyPrefix string) (start, end int, ok bool) {
	masked := maskJSONComments(content)
	key := strings.Trim(keyPrefix, `"`)
	dec := json.NewDecoder(bytes.NewReader(masked))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false
	}
	if d, isDelim := tok.(json.Delim); !isDelim || d != '{' {
		return 0, 0, false
	}
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		offsetAfterKey := int(dec.InputOffset())
		if k, isString := ktok.(string); isString && k == key {
			// The key token starts at offsetAfterKey - len(key) - 2 (quotes).
			keyStart := offsetAfterKey - len(key) - 2
			if keyStart < 0 {
				return 0, 0, false
			}
			// Skip the whitespace after the colon to find the value start.
			j := keyStart + len(key) + 2
			for j < len(masked) && (masked[j] == ' ' || masked[j] == '\t' || masked[j] == '\r' || masked[j] == '\n') {
				j++
			}
			if j >= len(masked) || masked[j] != ':' {
				return 0, 0, false
			}
			start = j + 1
			trimmed := bytes.TrimLeft(masked[start:], " \t\r\n")
			valStart := start + (len(masked[start:]) - len(trimmed))
			vdec := json.NewDecoder(bytes.NewReader(trimmed))
			var raw json.RawMessage
			if err := vdec.Decode(&raw); err != nil {
				return 0, 0, false
			}
			end = valStart + len(raw)
			return valStart, end, true
		}
		// Skip this key's value (nested objects/arrays included).
		if err := skipJSONValue(dec); err != nil {
			return 0, 0, false
		}
	}
	return 0, 0, false
}

// skipJSONValue advances dec past one JSON value, descending into any nested
// object or array.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, isDelim := tok.(json.Delim)
	if !isDelim {
		return nil // scalar
	}
	var closing rune
	switch d {
	case '{':
		closing = '}'
	case '[':
		closing = ']'
	default:
		return nil
	}
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if dd, isDelim := t.(json.Delim); isDelim && dd == json.Delim(closing) {
			return nil
		}
	}
}

// jsonValid reports whether content is valid JSON allowing JSONC comments.
func jsonValid(content []byte) bool {
	return json.Valid(maskJSONComments(content))
}

// replaceJSONValue splices newValue (already a JSON literal) over the value of
// keyPrefix. Returns the new content, whether a replacement happened, and an
// error when the file is not valid JSON.
func replaceJSONValue(content []byte, keyPrefix, newValue string) ([]byte, bool, error) {
	if !jsonValid(content) {
		return nil, false, fmt.Errorf("配置文件不是合法 JSON（允许 // 与 /* */ 注释）")
	}
	start, end, ok := jsonValueRange(content, keyPrefix)
	if !ok {
		return content, false, nil
	}
	out := make([]byte, 0, len(content)+len(newValue))
	out = append(out, content[:start]...)
	out = append(out, newValue...)
	out = append(out, content[end:]...)
	return out, true, nil
}

// ensureJSONKey appends "key": value before the root object's closing brace
// when the key is missing. The trailing-comment form (`{...}\n// note`) keeps
// its comment after the closing brace, so the closing brace is located by a
// depth scan on the masked copy, never by assuming the last character.
func ensureJSONKey(content []byte, key, value string) ([]byte, bool, error) {
	if !jsonValid(content) {
		return nil, false, fmt.Errorf("配置文件不是合法 JSON（允许 // 与 /* */ 注释）")
	}
	close := rootCloseOffset(maskJSONComments(content))
	if close < 0 {
		return nil, false, fmt.Errorf("配置文件必须是 JSON 对象")
	}
	inner := bytes.TrimSpace(content[1:close])
	sep := ","
	if len(inner) == 0 {
		sep = ""
	}
	pair := sep + "\n  " + strconv.Quote(key) + ":" + value + "\n"
	out := make([]byte, 0, len(content)+len(pair))
	out = append(out, content[:close]...)
	out = append(out, pair...)
	out = append(out, content[close:]...)
	return out, true, nil
}

// rootCloseOffset returns the byte offset of the root object's closing brace,
// or -1 when the content is not an object. The mask is comment-free; string
// contents are still tracked so braces inside strings never count.
func rootCloseOffset(masked []byte) int {
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(masked); i++ {
		c := masked[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// jsonString quotes v as a JSON string literal.
func jsonString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// jsonObject builds a JSON object literal from ordered pairs.
func jsonObject(pairs ...string) string {
	return "{" + strings.Join(pairs, ",") + "}"
}

// PatchClaudeCode sets env.ANTHROPIC_BASE_URL, env.ANTHROPIC_AUTH_TOKEN and
// model in a Claude Code settings.json, preserving all other fields.
func PatchClaudeCode(content []byte, baseURL, gatewayKey, model string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(maskJSONComments(content), &doc); err != nil {
		return nil, fmt.Errorf("~/.claude/settings.json 不是合法 JSON：%w", err)
	}
	env, _ := doc["env"].(map[string]any)
	envPairs := []string{}
	for k, v := range env {
		if k == "ANTHROPIC_BASE_URL" || k == "ANTHROPIC_AUTH_TOKEN" {
			continue
		}
		envPairs = append(envPairs, strconv.Quote(k)+":"+marshalAny(v))
	}
	envPairs = append(envPairs,
		strconv.Quote("ANTHROPIC_BASE_URL")+":"+jsonString(baseURL),
		strconv.Quote("ANTHROPIC_AUTH_TOKEN")+":"+jsonString(gatewayKey),
	)
	sortStrings(envPairs)
	newEnv := jsonObject(envPairs...)
	var err error
	out, _, err := replaceJSONValue(content, `"env"`, newEnv)
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(out, []byte(`"env"`)) {
		out, _, err = ensureJSONKey(out, "env", newEnv)
		if err != nil {
			return nil, err
		}
	}
	if model != "" {
		var changed bool
		if out, changed, err = replaceJSONValue(out, `"model"`, jsonString(model)); err != nil {
			return nil, err
		}
		if !changed {
			if out, _, err = ensureJSONKey(out, "model", jsonString(model)); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// PatchOpenCode sets the model (and small_model when given) in an OpenCode
// settings JSON file.
func PatchOpenCode(content []byte, model, smallModel string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(maskJSONComments(content), &doc); err != nil {
		return nil, fmt.Errorf("opencode.json 不是合法 JSON：%w", err)
	}
	out := content
	var changed bool
	var err error
	if out, changed, err = replaceJSONValue(out, `"model"`, jsonString(model)); err != nil {
		return nil, err
	}
	if !changed {
		if out, _, err = ensureJSONKey(out, "model", jsonString(model)); err != nil {
			return nil, err
		}
	}
	if smallModel != "" {
		if out, changed, err = replaceJSONValue(out, `"small_model"`, jsonString(smallModel)); err != nil {
			return nil, err
		}
		if !changed {
			if out, _, err = ensureJSONKey(out, "small_model", jsonString(smallModel)); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// PatchCline sets the flat cline.* keys in a VS Code settings.json.
func PatchCline(content []byte, baseURL, model, gatewayKey string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(maskJSONComments(content), &doc); err != nil {
		return nil, fmt.Errorf("VS Code settings.json 不是合法 JSON：%w", err)
	}
	pairs := [][2]string{
		{"cline.apiProvider", jsonString("openai-compatible")},
		{"cline.openAiBaseUrl", jsonString(baseURL + "/v1")},
		{"cline.openAiModelId", jsonString(model)},
		{"cline.openAiKey", jsonString(gatewayKey)},
	}
	out := content
	for _, p := range pairs {
		replaced := false
		var err error
		if out, replaced, err = replaceJSONValue(out, strconv.Quote(p[0]), p[1]); err != nil {
			return nil, err
		}
		if !replaced {
			if out, _, err = ensureJSONKey(out, p[0], p[1]); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// PatchCodex edits a Codex config.toml: sets model_provider to "magpie", the
// model line, and creates/replaces the [model_providers.magpie] section.
func PatchCodex(content []byte, baseURL, model string) ([]byte, error) {
	lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
	providerSet := false
	modelSet := false
	sectionStart := -1
	sectionEnd := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "model_provider"):
			lines[i] = replaceAssign(line, "model_provider", `"magpie"`)
			providerSet = true
		case strings.HasPrefix(trimmed, "model"):
			// Only the top-level model key (not model_provider).
			if !strings.HasPrefix(trimmed, "model_") {
				lines[i] = replaceAssign(line, "model", strconv.Quote(model))
				modelSet = true
			}
		case strings.HasPrefix(trimmed, "["):
			if sectionStart >= 0 && sectionEnd < 0 {
				sectionEnd = i
			}
			if strings.Trim(trimmed, "[]\"") == "model_providers.magpie" {
				sectionStart = i
			}
		}
	}
	if sectionStart >= 0 && sectionEnd < 0 {
		sectionEnd = len(lines)
	}
	// Build the canonical magpie section.
	magpieSection := []string{
		"[model_providers.magpie]",
		"name = \"magpie\"",
		"base_url = " + strconv.Quote(baseURL+"/v1"),
		"wire_api = \"responses\"",
		"env_key = \"MAGPIE_API_KEY\"",
	}
	if sectionStart >= 0 {
		// Replace the existing section body (between header and next header).
		lines = append(lines[:sectionStart+1], append(magpieSection[1:], lines[sectionEnd:]...)...)
	} else {
		lines = append(lines, "")
		lines = append(lines, magpieSection...)
	}
	if !providerSet {
		// Insert near the top, after any initial comments/blank lines.
		insertAt := 0
		for insertAt < len(lines) && (strings.TrimSpace(lines[insertAt]) == "" || strings.HasPrefix(strings.TrimSpace(lines[insertAt]), "#")) {
			insertAt++
		}
		lines = append(lines[:insertAt], append([]string{`model_provider = "magpie"`}, lines[insertAt:]...)...)
	}
	if !modelSet {
		// Put the model right after model_provider.
		insertAt := 0
		for insertAt < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[insertAt]), "model_provider") {
			insertAt++
		}
		if insertAt < len(lines) {
			lines = append(lines[:insertAt+1], append([]string{`model = ` + strconv.Quote(model)}, lines[insertAt+1:]...)...)
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// replaceAssign rewrites "key = value" on a line preserving leading whitespace.
func replaceAssign(line, key, value string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, key+"=") {
		idx := strings.Index(line, "=")
		return line[:idx+1] + " " + value
	}
	// "key = value" with spaces before '='.
	if idx := strings.Index(line, key); idx >= 0 {
		eq := strings.Index(line[idx:], "=")
		if eq >= 0 {
			return line[:idx+eq+1] + " " + value
		}
	}
	return line
}

func marshalAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---- file operations: backup, atomic write, restore ----

// ApplyResult describes one successful file write.
type ApplyResult struct {
	TargetFile string `json:"target_file"`
	BackupID   string `json:"backup_id,omitempty"`
	BackedUp   bool   `json:"backed_up"`
	Created    bool   `json:"created"`
}

// WriteFile backs up the previous file (if any) into the store backup
// directory, then writes content atomically (temp + rename) with 0600.
// Backups are pruned to the newest maxBackupsPerFile per base name so the
// directory cannot grow without bound.
func (s *Store) WriteFile(target, backupID string, content []byte) (*ApplyResult, error) {
	if err := os.MkdirAll(s.Dir(), 0700); err != nil {
		return nil, fmt.Errorf("backup directory: %w", err)
	}
	res := &ApplyResult{TargetFile: target}
	if old, err := os.ReadFile(target); err == nil {
		id := backupID
		if id == "" {
			id = fmt.Sprintf("%s-%d", filepath.Base(target), time.Now().UnixNano())
		}
		backup := filepath.Join(s.Dir(), id)
		if err := os.WriteFile(backup, old, 0600); err != nil {
			return nil, fmt.Errorf("backup write: %w", err)
		}
		res.BackupID = id
		res.BackedUp = true
		s.pruneBackups(filepath.Base(target), maxBackupsPerFile)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取现有配置：%w", err)
	} else {
		res.Created = true
	}
	tmp := target + ".daydayup-tmp"
	if err := os.WriteFile(tmp, content, 0600); err != nil {
		return nil, fmt.Errorf("配置写入：%w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return nil, fmt.Errorf("配置替换：%w", err)
	}
	return res, nil
}

// maxBackupsPerFile caps how many backups are kept for one config file.
const maxBackupsPerFile = 5

// pruneBackups removes the oldest backups whose name starts with base, keeping
// the newest maxBackupsPerFile entries.
func (s *Store) pruneBackups(base string, keep int) {
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		return
	}
	type backupEntry struct {
		name string
		mod  time.Time
	}
	var matches []backupEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), base+"-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		matches = append(matches, backupEntry{name: e.Name(), mod: info.ModTime()})
	}
	if len(matches) <= keep {
		return
	}
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && matches[j].mod.Before(matches[j-1].mod); j-- {
			matches[j], matches[j-1] = matches[j-1], matches[j]
		}
	}
	for _, m := range matches[:len(matches)-keep] {
		_ = os.Remove(filepath.Join(s.Dir(), m.name))
	}
}

// RestoreFile writes the backup back over target.
func (s *Store) RestoreFile(target, backupID string) error {
	if backupID == "" {
		return fmt.Errorf("没有可还原的备份")
	}
	backup := filepath.Join(s.Dir(), backupID)
	b, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("读取备份：%w", err)
	}
	tmp := target + ".daydayup-tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// ReadConfig returns the current config file content for a resolved target
// (empty content when the file does not exist yet).
func ReadConfig(target string) ([]byte, error) {
	b, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}