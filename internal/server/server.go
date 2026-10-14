// Package server assembles the daemon's HTTP handler.
package server

import (
	"net"
	"net/http"
	"strings"

	"github.com/Revanth14/agent-tap/internal/telemetry"
)

// New returns the daemon handler for a listener on addr (host:port). The
// daemon's own exact routes, the hub's, the UI's under /ui/, and the adapter
// routes (pattern -> handler), are served locally; every other path goes to proxy, because Claude
// Code sends non-/v1 paths such as HEAD /api/hello to the base URL. Everything
// sits behind the Host check.
func New(addr string, proxy http.Handler, hub *telemetry.Hub, routes map[string]http.Handler) http.Handler {
	mux := http.NewServeMux()
	hub.Register(mux)
	for pattern, h := range routes {
		mux.Handle(pattern, h)
	}
	mux.Handle("GET /ui/", uiHandler(embeddedUI()))
	mux.Handle("GET /ui", http.RedirectHandler("/ui/", http.StatusMovedPermanently))
	mux.Handle("/", proxy)
	return hostGuard(addr, mux)
}

// hostGuard rejects requests whose Host is not this daemon's loopback
// address. That defeats DNS rebinding: a hostile page whose domain resolves
// to 127.0.0.1 still sends its own domain as Host, so it cannot read
// /api/state even though the browser treats it as same-origin.
func hostGuard(addr string, next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(addr)
	allowed := map[string]bool{
		"127.0.0.1:" + port: true,
		"localhost:" + port: true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "atap: forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
