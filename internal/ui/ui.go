// Package ui serves the interface: the SvelteKit build embedded in the binary.
package ui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the build in fsys. A path that names no file gets index.html,
// so the interface routes it. Without a build it answers 503 with how to make
// one, rather than a blank page.
func Handler(fsys fs.FS) http.Handler {
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("The interface is not built into this binary: run make ui, then rebuild.\n"))
		})
	}
	csp := policy(index)
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if st, err := fs.Stat(fsys, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "_app/immutable/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			// A missing asset is a 404, not the page: a stale chunk must fail loudly.
			if path.Ext(p) != "" {
				http.NotFound(w, r)
				return
			}
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}

// Embedded is the build compiled into the binary.
func Embedded() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the directory is part of the binary
	}
	return Handler(sub)
}

var inlineScript = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// policy allows the page's own files and, by hash, the inline script
// SvelteKit starts the app with, and nothing else: screenshots of targets and
// their titles are shown here, so no other origin and no other inline script.
func policy(index []byte) string {
	hashes := []string{}
	for _, m := range inlineScript.FindAllSubmatch(index, -1) {
		sum := sha256.Sum256(m[1])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	var b bytes.Buffer
	b.WriteString("default-src 'self'; script-src 'self'")
	for _, h := range hashes {
		b.WriteString(" " + h)
	}
	b.WriteString("; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; ")
	b.WriteString("object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	return b.String()
}
