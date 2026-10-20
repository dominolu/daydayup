package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, code, msg string) {
	markGatewayError(w, status, code, msg)
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": "gateway_error", "code": code, "message": msg}})
}
func authorized(r *http.Request, key string) bool {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if value == "" {
		value = r.Header.Get("X-API-Key")
	}
	return key != "" && subtle.ConstantTimeCompare([]byte(value), []byte(key)) == 1
}

// remoteIP returns the direct peer address (no X-Forwarded-For: the admin
// surface is single-process and trusting client-supplied IPs would let an
// attacker spoof the OAuth rate limit).
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func validPath(p string) bool {
	if strings.ContainsAny(p, "\\\x00\r\n") || strings.Contains(p, "//") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func retryDelay(value string, fallback time.Duration) time.Duration {
	if n, err := strconv.ParseInt(value, 10, 32); err == nil && n >= 0 {
		d := time.Duration(n) * time.Second
		if d > 24*time.Hour {
			return 24 * time.Hour
		}
		return d
	}
	if t, err := http.ParseTime(value); err == nil {
		d := time.Until(t)
		if d > 24*time.Hour {
			return 24 * time.Hour
		}
		if d > 0 {
			return d
		}
	}
	return fallback
}
func cooldown(status int, retryAfter string) time.Duration {
	if status == 429 {
		return retryDelay(retryAfter, 30*time.Second)
	}
	if status >= 500 {
		return retryDelay(retryAfter, 5*time.Second)
	}
	return 0
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		apiError(w, 400, "invalid_json", "invalid JSON request body")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		apiError(w, 400, "invalid_json", "expected one JSON object")
		return false
	}
	return true
}
