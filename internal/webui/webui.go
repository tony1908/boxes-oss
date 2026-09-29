package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var assets embed.FS

// Handler serves the exported dashboard and falls back to its entry point for
// client-side application routes that are not present in the static export.
func Handler() http.Handler {
	site, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(site))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_next/static/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "." || name == "" || name == "index.html" {
			r.URL.Path = "/"
			files.ServeHTTP(w, r)
			return
		}
		info, err := fs.Stat(site, name)
		if err != nil {
			r.URL.Path = "/"
		} else {
			r.URL.Path = "/" + name
			if info.IsDir() {
				r.URL.Path += "/"
			}
		}
		files.ServeHTTP(w, r)
	})
}
