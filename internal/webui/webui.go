// Package webui embeds the built web frontend (web/, Vite + React): the
// hub's panel and longarm-tray's pages are the same bundle. dist/ is committed so
// `go build` works without node; regenerate it with `go generate
// ./internal/webui` (runs web/build.mjs: pnpm install + vite build + copy).
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:generate node ../../web/build.mjs

//go:embed all:dist
var dist embed.FS

// FS is the built frontend (index.html, assets/...).
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

// Handler serves the frontend: files as they are (hashed assets are
// cached for a year), anything else as index.html (client-side routes).
// Paths under any of the excluded prefixes are not handled (404).
func Handler(exclude ...string) http.Handler {
	root := FS()
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		for _, p := range exclude {
			if strings.HasPrefix(r.URL.Path, p) {
				http.NotFound(w, r)
				return
			}
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if fi, err := fs.Stat(root, p); err == nil && !fi.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			if path.Ext(p) != "" { // a missing file, not a route
				http.NotFound(w, r)
				return
			}
		}
		b, err := fs.ReadFile(root, "index.html")
		if err != nil {
			http.Error(w, "web UI not built (go generate ./internal/webui)", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	})
}
