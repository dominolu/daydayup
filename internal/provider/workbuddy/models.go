package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"daydayup/internal/config"
	wbp "daydayup/internal/provider"
)

const remoteConfigPath = "/v3/config"

// Models returns the curated model list for the account realm: the live
// product-config endpoint when available, falling back to the bundled
// snapshot (desktop client parity). Unusable credentials answer HTTP 200 with
// an empty list, which is reported as a discovery failure so the fallback
// keeps being advertised instead of "this exit has no models".
func (w WorkBuddy) Models(ctx context.Context, client *http.Client, a config.Account) ([]wbp.Model, error) {
	rc := realmOf(a)
	if live, err := w.fetchRemoteCatalog(ctx, client, a, rc); err == nil && len(live) > 0 {
		return mergeCatalog(isCN(a), live), nil
	}
	return bundledCatalog(isCN(a)), nil
}

func (w WorkBuddy) fetchRemoteCatalog(ctx context.Context, client *http.Client, a config.Account, rc realmConfig) ([]wbp.Model, error) {
	chat := rc.chatUpstream
	if a.BaseURL != "" && a.BaseURL != rc.chatUpstream {
		chat = a.BaseURL
	}
	url := chat + remoteConfigPath
	h := map[string]string{
		"Accept":        "application/json, text/plain, */*",
		"User-Agent":    rc.chatUA,
		"Origin":        rc.origin,
		"Referer":       rc.origin + "/",
		"Authorization": "Bearer " + a.APIKey,
		"X-User-Id":     uidOf(a),
	}
	b, status, err := doJSON(ctx, client, http.MethodGet, url, h, nil, 4<<20)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &wbp.HTTPError{Status: status, RetryAfter: ""}
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(b, &root) != nil {
		return nil, errors.New("invalid product-config response")
	}
	// agents[].models[] bare-id lists: the cli-named agent first, longest list.
	var cliIDs, otherIDs []string
	data := rawDict(root["data"])
	for _, doc := range []map[string]json.RawMessage{root, data} {
		if doc == nil {
			continue
		}
		if agents, ok := asObject(doc["agents"]); ok {
			for name, entry := range agents {
				m := rawDict(entry)
				if m == nil {
					continue
				}
				if models, ok := bareIDList(m["models"]); ok && len(models) > 0 {
					if strings.EqualFold(name, "cli") {
						cliIDs = maxList(cliIDs, models)
					} else {
						otherIDs = maxList(otherIDs, models)
					}
				}
			}
		}
		if agents, ok := asArray(doc["agents"]); ok {
			for _, entry := range agents {
				e, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				modelsRaw, _ := json.Marshal(e["models"])
				if models, ok := bareIDList(modelsRaw); ok && len(models) > 0 {
					if strings.EqualFold(fmt.Sprint(e["name"]), "cli") {
						cliIDs = maxList(cliIDs, models)
					} else {
						otherIDs = maxList(otherIDs, models)
					}
				}
			}
		}
	}
	pool := cliIDs
	if len(otherIDs) > len(pool) {
		pool = otherIDs
	}
	// Per-model metadata rides in arbitrary models[] dict arrays; walk the
	// whole document so the position never matters.
	meta := map[string]wbp.Model{}
	walkModelDicts(b, meta)
	if len(pool) == 0 {
		return nil, errors.New("product-config returned no models")
	}
	curated := curateLive(pool, meta)
	out := make([]wbp.Model, 0, len(curated))
	for _, mid := range curated {
		if m, ok := meta[mid]; ok {
			c := wbp.Model{}
			for k, v := range m {
				c[k] = v
			}
			out = append(out, c)
		} else {
			out = append(out, wbp.Model{"id": mid})
		}
	}
	return out, nil
}

// ---- catalog curation ----

var variantSuffixes = []string{"-sg", "-x"}

// isChatModel filters completion-only and multi-cloud line variants that the
// desktop picker never advertises as chat models, plus the virtual picker
// aliases (Auto / Fast / Balanced / Primary / Deep are not model ids).
func isChatModel(id string) bool {
	if id == "" {
		return false
	}
	if aliasModels[id] {
		return false
	}
	if strings.HasPrefix(id, "codewise-") || strings.HasPrefix(id, "completion-") {
		return false
	}
	lower := strings.ToLower(id)
	if strings.Contains(lower, "hunyuan-3b") || strings.Contains(lower, "hunyuan-7b") {
		return false
	}
	for _, suffix := range []string{"-volc", "-lkeap"} {
		if strings.HasSuffix(id, suffix) {
			return false
		}
	}
	return true
}

// aliasModels are the desktop picker's virtual aliases, not real model ids.
var aliasModels = map[string]bool{
	"default-model": true, "fast-model": true, "balanced-model": true,
	"primary-model": true, "deep-model": true, "default-1.1": true,
	"default-1.2": true,
}

// freeModel reports the payroll-style "x0.00" free multiplier.
func freeModel(credits string) bool {
	switch strings.ToLower(strings.TrimSpace(credits)) {
	case "x0.00", "x0", "0", "0.00":
		return true
	}
	return false
}

// curateLive trims a live model list with the desktop picker rules: drop
// variant builds, and when a free ("x0.00") sibling exists keep the free one.
func curateLive(ids []string, meta map[string]wbp.Model) []string {
	credits := map[string]string{}
	for _, m := range bundledAll() {
		if id, _ := m["id"].(string); id != "" {
			credits[id] = fmt.Sprint(m["credits"])
		}
	}
	for id, m := range meta {
		credits[id] = fmt.Sprint(m["credits"])
	}
	out := make([]string, 0, len(ids))
	for _, mid := range ids {
		if !isChatModel(mid) {
			continue
		}
		trimmed := mid
		for _, s := range variantSuffixes {
			trimmed = strings.TrimSuffix(trimmed, s)
		}
		if trimmed != mid {
			continue
		}
		if strings.HasSuffix(mid, "-f") {
			base := strings.TrimSuffix(mid, "-f")
			if freeModel(credits[base]) && !freeModel(credits[mid]) {
				continue
			}
		} else if sibling := mid + "-f"; freeModel(credits[sibling]) && !freeModel(credits[mid]) {
			continue
		}
		out = append(out, mid)
	}
	return out
}

// mergeCatalog overlays the curated live ids onto the bundled snapshot: the
// bundled order and metadata win where they coincide; brand-new live ids are
// appended bare so fresh models appear without a gateway release.
func mergeCatalog(cn bool, live []wbp.Model) []wbp.Model {
	seen := map[string]bool{}
	out := []wbp.Model{}
	for _, m := range bundledCatalog(cn) {
		id, _ := m["id"].(string)
		seen[id] = true
		c := wbp.Model{}
		for k, v := range m {
			c[k] = v
		}
		c["source"] = "catalog"
		out = append(out, c)
	}
	for _, m := range live {
		id, _ := m["id"].(string)
		if id == "" || seen[id] {
			continue
		}
		c := wbp.Model{}
		for k, v := range m {
			c[k] = v
		}
		c["source"] = "live"
		out = append(out, c)
	}
	return out
}

func rawDict(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	return nil
}

func asObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	m := rawDict(raw)
	return m, m != nil
}

func asArray(raw json.RawMessage) ([]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var a []any
	if json.Unmarshal(raw, &a) == nil {
		return a, true
	}
	return nil, false
}

func bareIDList(raw json.RawMessage) ([]string, bool) {
	var a []string
	if json.Unmarshal(raw, &a) == nil && len(a) > 0 {
		return a, true
	}
	return nil, false
}

func maxList(a, b []string) []string {
	if len(b) > len(a) {
		return b
	}
	return a
}

// walkModelDicts collects every models[] array of dicts with an id field.
func walkModelDicts(b []byte, out map[string]wbp.Model) {
	var root any
	if json.Unmarshal(b, &root) != nil {
		return
	}
	var walk func(any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if models, ok := v["models"].([]any); ok {
				for _, item := range models {
					if m, ok := item.(map[string]any); ok {
						if id, ok := m["id"].(string); ok && id != "" {
							raw, _ := json.Marshal(m)
							var model wbp.Model
							if json.Unmarshal(raw, &model) == nil {
								out[id] = model
							}
						}
					}
				}
			}
			for _, value := range v {
				walk(value)
			}
		case []any:
			for _, value := range v {
				walk(value)
			}
		}
	}
	walk(root)
}
