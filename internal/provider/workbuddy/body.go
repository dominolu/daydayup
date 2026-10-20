package workbuddy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"

	"daydayup/internal/config"
)

// cliBodyDropFields are the Chat-Completions fields the official CLI never
// sends (Responses-API state fields and client metadata).
var cliBodyDropFields = []string{"metadata", "conversation_id", "previous_response_id"}

// TransformRequestBody rewrites chat JSON bodies for the CLI/VSCode
// identities (official RequestBodyGzip behavior): gzip + compact JSON. The
// CLI identity additionally injects cache_retention="long" and drops the
// Responses-API fields the official CLI never carries. Desktop identity and
// non-chat routes pass through unchanged.
func (WorkBuddy) TransformRequestBody(method, path string, contentType string, body []byte, a config.Account) ([]byte, error) {
	if method != http.MethodPost || path != "/v1/chat/completions" {
		return body, nil
	}
	p := config.DefaultProduct(a)
	if p != config.ProductCLI && p != config.ProductVSCode {
		return body, nil
	}
	if ct, _, _ := strings.Cut(contentType, ";"); !strings.EqualFold(ct, "application/json") {
		return body, nil
	}
	if p == config.ProductCLI {
		var payload map[string]json.RawMessage
		if json.Unmarshal(body, &payload) == nil && payload != nil {
			for _, key := range cliBodyDropFields {
				delete(payload, key)
			}
			payload["cache_retention"] = json.RawMessage(`"long"`)
			if rewritten, err := json.Marshal(payload); err == nil {
				body = rewritten
			}
		}
	}
	return gzipBytes(body)
}

func gzipBytes(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// BodyEncodingHeaders signals Content-Encoding for gzipped bodies.
func (WorkBuddy) BodyEncodingHeaders(a config.Account) http.Header {
	p := config.DefaultProduct(a)
	if p == config.ProductCLI || p == config.ProductVSCode {
		h := http.Header{}
		h.Set("Content-Encoding", "gzip")
		return h
	}
	return nil
}
