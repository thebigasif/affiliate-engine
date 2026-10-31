// Package ui embeds the admin console + affiliate portal (FR-27/28).
// Single-binary philosophy: the binary serves its own UI from embed.FS.
package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

//go:embed static/*
var staticFS embed.FS

// Handler returns the UI file server (serves /static/* from the embed.FS).
// The shell links each script as /static/<file>?v=<content hash>, so a
// versioned URL never changes content and may be cached for a year. An
// unversioned request must revalidate: without an explicit header a CDN
// (Cloudflare adds max-age=14400) kept an old app.js behind a new shell.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("ui: embedded static missing: " + err.Error())
	}
	files := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

var (
	indexOnce sync.Once
	indexHTML []byte
	indexErr  error
)

// versionedIndex is index.html with each /static script URL carrying a
// short hash of that file's bytes.
func versionedIndex() ([]byte, error) {
	indexOnce.Do(func() {
		data, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			indexErr = err
			return
		}
		html := string(data)
		for _, name := range []string{"boot.js", "app.js"} {
			b, err := staticFS.ReadFile("static/" + name)
			if err != nil {
				indexErr = err
				return
			}
			sum := sha256.Sum256(b)
			html = strings.ReplaceAll(html, `src="/static/`+name+`"`, `src="/static/`+name+`?v=`+hex.EncodeToString(sum[:])[:12]+`"`)
		}
		indexHTML = []byte(html)
	})
	return indexHTML, indexErr
}

// contentSecurityPolicy: every script is an embedded same-origin file
// (/static/boot.js, /static/app.js), so script-src needs no
// 'unsafe-inline' — an injected <script> or inline handler never runs.
// Inline style attributes remain (cosmetic only).
const contentSecurityPolicy = "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; img-src 'self' data: blob: https:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

// IndexHandler serves the single-page shell for /, /admin, /portal.
func IndexHandler(w http.ResponseWriter, r *http.Request) {
	data, err := versionedIndex()
	if err != nil {
		http.Error(w, "ui: index missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write(data)
}
