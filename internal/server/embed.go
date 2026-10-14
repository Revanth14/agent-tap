package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// dist holds the built web UI. The Makefile copies web/dist here before
// go build; on a fresh clone it holds only .gitkeep.
//
//go:embed all:dist
var dist embed.FS

func embeddedUI() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is always present: it is embedded.
	}
	return sub
}

// uiHandler serves the built UI under /ui/. The UI renders prompt text and
// raw payloads taken from traffic, so a strict Content-Security-Policy keeps
// any markup in them inert even if a rendering path ever failed to escape it.
func uiHandler(files fs.FS) http.Handler {
	if _, err := fs.Stat(files, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "atap: the web UI is not built into this binary; run `make build`", http.StatusServiceUnavailable)
		})
	}
	fileServer := http.StripPrefix("/ui/", http.FileServerFS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src 'self' ws://"+r.Host+"; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
