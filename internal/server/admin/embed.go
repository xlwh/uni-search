// Package admin implements the UniSearch web admin console server.
// It serves an embedded Vue 3 SPA on a separate port and proxies ES API
// requests to the main REST server via httputil.ReverseProxy.
package admin

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed dist
var distFS embed.FS

// staticHandler serves embedded frontend assets from the dist/ directory.
// Non-existent paths fall back to index.html for SPA client-side routing.
type staticHandler struct {
	fileServer http.Handler
	fsys       fs.FS
}

func newStaticHandler() *staticHandler {
	fsys, _ := fs.Sub(distFS, "dist")
	return &staticHandler{
		fileServer: http.FileServer(http.FS(fsys)),
		fsys:       fsys,
	}
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}

	// If the file exists, serve it directly.
	if _, err := fs.Stat(h.fsys, path); err == nil {
		h.fileServer.ServeHTTP(w, r)
		return
	}

	// SPA fallback: serve index.html for client-side routing.
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/"
	h.fileServer.ServeHTTP(w, r2)
}
