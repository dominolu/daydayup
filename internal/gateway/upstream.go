package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

func stripHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			h.Del(strings.TrimSpace(key))
		}
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}
func requestHeaders(h http.Header) http.Header {
	h = h.Clone()
	h.Del("Content-Length")
	stripHeaders(h)
	for _, key := range []string{"Authorization", "X-API-Key", "Cookie", "X-Provider", "X-Group", "X-Admin-Key", "X-Session-ID", "X-Session-Source", "X-Request-Purpose", "X-WorkBuddy-Realm", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "Accept-Encoding"} {
		h.Del(key)
	}
	h.Set("Accept-Encoding", "identity")
	return h
}
func modelFrom(r *http.Request, b []byte) string {
	model := r.URL.Query().Get("model")
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" || strings.HasSuffix(ct, "+json") {
		var body struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(b, &body) == nil && body.Model != "" {
			model = body.Model
		}
	} else if ct == "multipart/form-data" && params["boundary"] != "" {
		reader := multipart.NewReader(bytes.NewReader(b), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			if part.FormName() == "model" && part.FileName() == "" {
				v, err := io.ReadAll(io.LimitReader(part, 1025))
				if err == nil && len(v) <= 1024 {
					model = string(v)
				}
				part.Close()
				break
			}
			part.Close()
		}
	} else if ct == "application/x-www-form-urlencoded" {
		if v, err := url.ParseQuery(string(b)); err == nil && v.Get("model") != "" {
			model = v.Get("model")
		}
	}
	return model
}

// rewriteModelField returns body with its top-level "model" field set to
// model. It is used by model_rename to hand each platform the id it actually
// advertises. Non-JSON bodies and bodies without a JSON object are returned
// unchanged (multipart /v1/images/edits keeps its original model field).
func rewriteModelField(body []byte, model string) []byte {
	if len(body) == 0 || model == "" {
		return body
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return body
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return body
	}
	root["model"] = raw
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}
