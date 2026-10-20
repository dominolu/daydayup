package gateway

import (
	"bytes"
	"embed"
	"mime"
	"net/http"
	"path"
	"time"
)

//go:embed web/*
var webFiles embed.FS

func serveAdminUI(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return true
	}
	name := ""
	switch r.URL.Path {
	case "/admin/":
		name = "index.html"
	case "/admin/assets/app.css":
		name = "app.css"
	case "/admin/assets/app.js":
		name = "app.js"
	default:
		return false
	}
	b, err := webFiles.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
	return true
}
