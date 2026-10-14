package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Revanth14/agent-tap/internal/telemetry"
)

// newTestServer starts the daemon handler with a stub proxy that records the
// paths it receives.
func newTestServer(t *testing.T) (srv *httptest.Server, proxied chan string) {
	t.Helper()
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	proxied = make(chan string, 10)
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied <- r.Method + " " + r.URL.Path
	})
	raw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Raw-Span", r.PathValue("id"))
	})
	srv = httptest.NewUnstartedServer(nil)
	srv.Config.Handler = New(srv.Listener.Addr().String(), proxy, telemetry.NewHub(store), map[string]http.Handler{"GET /api/spans/{id}/raw": raw})
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, proxied
}

func do(t *testing.T, method, url, host string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestRoutesDaemonPathsLocallyAndTheRestToProxy(t *testing.T) {
	srv, proxied := newTestServer(t)

	resp := do(t, http.MethodGet, srv.URL+"/api/state", "")
	var snap telemetry.StateSnapshot
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&snap) != nil {
		t.Errorf("GET /api/state = %d, want a snapshot served locally", resp.StatusCode)
	}

	if got := do(t, http.MethodGet, srv.URL+"/api/spans/msg_01/raw", "").Header.Get("X-Raw-Span"); got != "msg_01" {
		t.Errorf("GET /api/spans/msg_01/raw reached the raw handler with id %q, want msg_01", got)
	}

	for _, req := range []string{"HEAD /api/hello", "POST /v1/messages"} {
		method, path, _ := strings.Cut(req, " ")
		do(t, method, srv.URL+path, "")
		select {
		case got := <-proxied:
			if got != req {
				t.Errorf("proxy got %q, want %q", got, req)
			}
		default:
			t.Errorf("%s was not sent to the proxy", req)
		}
	}
	if len(proxied) != 0 {
		t.Errorf("unexpected proxied requests: %d", len(proxied))
	}
}

func TestHostGuard(t *testing.T) {
	srv, proxied := newTestServer(t)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	tests := []struct {
		host string
		want int
	}{
		{"", http.StatusOK}, // 127.0.0.1:<port>, as Claude Code sends it
		{fmt.Sprintf("localhost:%d", port), http.StatusOK},
		{fmt.Sprintf("LOCALHOST:%d", port), http.StatusOK},
		{fmt.Sprintf("evil.example:%d", port), http.StatusForbidden}, // DNS rebinding
		{"localhost", http.StatusForbidden},
		{fmt.Sprintf("127.0.0.1:%d", port+1), http.StatusForbidden},
	}
	for _, tt := range tests {
		for _, path := range []string{"/api/state", "/v1/messages"} {
			if got := do(t, http.MethodGet, srv.URL+path, tt.host).StatusCode; got != tt.want {
				t.Errorf("Host %q %s: status %d, want %d", tt.host, path, got, tt.want)
			}
		}
	}
	for len(proxied) > 0 {
		<-proxied
	}
}

func TestUIIsServedLocallyWithCSP(t *testing.T) {
	srv, proxied := newTestServer(t)
	resp := do(t, http.MethodGet, srv.URL+"/ui/", "")
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /ui/ = %d, want the UI or a not-built notice", resp.StatusCode)
	}
	if len(proxied) != 0 {
		t.Errorf("GET /ui/ went to the proxy")
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := noRedirect.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusMovedPermanently || r.Header.Get("Location") != "/ui/" {
		t.Errorf("GET /ui = %d -> %q, want a redirect to /ui/", r.StatusCode, r.Header.Get("Location"))
	}
}

func TestUIHandler(t *testing.T) {
	built := fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><title>agent-tap</title>")},
		"assets/index-x.js":  {Data: []byte("console.log(1)")},
		"assets/index-x.css": {Data: []byte("body{}")},
	}
	h := uiHandler(built)
	for path, wantType := range map[string]string{"/ui/": "text/html", "/ui/assets/index-x.js": "text/javascript", "/ui/assets/index-x.css": "text/css"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1:9090"
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), wantType) {
			t.Errorf("%s = %d %q, want 200 %s", path, rr.Code, rr.Header().Get("Content-Type"), wantType)
		}
		csp := rr.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "connect-src 'self' ws://127.0.0.1:9090") {
			t.Errorf("%s CSP = %q", path, csp)
		}
	}

	rr := httptest.NewRecorder()
	uiHandler(fstest.MapFS{".gitkeep": {}}).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "make build") {
		t.Errorf("unbuilt UI = %d %q, want 503 pointing at make build", rr.Code, rr.Body)
	}
}
