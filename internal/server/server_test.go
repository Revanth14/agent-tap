package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	srv.Config.Handler = New(srv.Listener.Addr().String(), proxy, raw, telemetry.NewHub(store))
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
