package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestProxy starts upstream and a proxy to it, returning both servers.
func newTestProxy(t *testing.T, upstream http.Handler, wire io.Writer) (proxy, up *httptest.Server) {
	t.Helper()
	up = httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	p, err := NewProxy(up.URL, wire)
	if err != nil {
		t.Fatal(err)
	}
	proxy = httptest.NewServer(p)
	t.Cleanup(proxy.Close)
	return proxy, up
}

func TestProxyForwardsBodyAndHeadersVerbatim(t *testing.T) {
	// Unusual key order and whitespace: any decode/re-encode would change these bytes.
	body := []byte("{\"stream\":true,  \"model\":\"claude-opus-4-5\",\n\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}] }")
	sent := http.Header{
		"Authorization":            {"Bearer test-token"},
		"Anthropic-Beta":           {"oauth-2025-04-20,context-1m"},
		"Anthropic-Version":        {"2023-06-01"},
		"User-Agent":               {"claude-cli/2.1.300 (external, claude-vscode)"},
		"Content-Type":             {"application/json"},
		"X-Claude-Code-Session-Id": {"sess-1"},
		"X-Stainless-Lang":         {"js"},
		"Accept-Encoding":          {"gzip, br"},
	}

	type received struct {
		path, query, host string
		header            http.Header
		body              []byte
	}
	got := make(chan received, 1)
	proxy, up := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- received{r.URL.Path, r.URL.RawQuery, r.Host, r.Header.Clone(), b}
	}), nil)

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages?beta=true", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = sent.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	r := <-got

	if !bytes.Equal(r.body, body) {
		t.Errorf("upstream body =\n%q\nwant\n%q", r.body, body)
	}
	if r.path != "/v1/messages" || r.query != "beta=true" {
		t.Errorf("upstream path, query = %q, %q; want /v1/messages, beta=true", r.path, r.query)
	}
	if upURL, _ := url.Parse(up.URL); r.host != upURL.Host {
		t.Errorf("upstream Host = %q, want %q", r.host, upURL.Host)
	}
	want := sent.Clone()
	want.Set("Accept-Encoding", "identity")
	r.header.Del("Content-Length") // framing, checked via the body above
	if !reflect.DeepEqual(r.header, want) {
		t.Errorf("upstream headers =\n%v\nwant\n%v", r.header, want)
	}
}

func TestProxyStreamsSSEWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	proxy, _ := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		// Hold the stream open until the client has seen the first event.
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	}), nil)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || line != "event: message_start\n" {
		t.Fatalf("first line = %q, %v; want the first event before upstream finishes", line, err)
	}
	once.Do(func() { close(release) })
	rest, err := io.ReadAll(br)
	if err != nil || !strings.Contains(string(rest), "event: message_stop") {
		t.Errorf("rest of stream = %q, %v; want message_stop", rest, err)
	}
}

// syncBuffer is a bytes.Buffer safe for the proxy to write while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestWireLogOmitsCredentialsAndBodies(t *testing.T) {
	var wire syncBuffer
	proxy, _ := newTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.018")
		w.Header().Set("Set-Cookie", "session=SETCOOKIE-SECRET")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"RESPONSE-BODY-SECRET"}`)
	}), &wire)

	body := `{"model":"claude-opus-4-5","messages":[{"role":"user","content":"REQUEST-BODY-SECRET"}]}`
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer AUTH-SECRET")
	req.Header.Set("X-Api-Key", "APIKEY-SECRET")
	req.Header.Set("Cookie", "c=COOKIE-SECRET")
	req.Header.Set("User-Agent", "claude-cli/2.1.300 (external, cli)")
	req.Header.Set("X-Claude-Code-Session-Id", "sess-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The 429 passes through unchanged.
	if resp.StatusCode != http.StatusTooManyRequests ||
		resp.Header.Get("Anthropic-Ratelimit-Unified-5h-Utilization") != "0.018" ||
		string(respBody) != `{"error":"RESPONSE-BODY-SECRET"}` {
		t.Errorf("client got %d %v %q; want the upstream 429 unchanged", resp.StatusCode, resp.Header, respBody)
	}

	log := wire.String()
	for _, secret := range []string{"AUTH-SECRET", "APIKEY-SECRET", "COOKIE-SECRET", "SETCOOKIE-SECRET", "REQUEST-BODY-SECRET", "RESPONSE-BODY-SECRET"} {
		if strings.Contains(log, secret) {
			t.Errorf("wire log contains %s:\n%s", secret, log)
		}
	}

	var entries []wireEntry
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var e wireEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("wire log line %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	if len(entries) != 2 {
		t.Fatalf("wire log has %d entries, want 2:\n%s", len(entries), log)
	}
	reqE, respE := entries[0], entries[1]
	if reqE.Dir != "request" || reqE.Model != "claude-opus-4-5" || reqE.Headers["X-Claude-Code-Session-Id"] != "sess-1" ||
		reqE.Headers["User-Agent"] != "claude-cli/2.1.300 (external, cli)" || !slices.Contains(reqE.HeaderNames, "Authorization") {
		t.Errorf("request entry = %+v", reqE)
	}
	if respE.Dir != "response" || respE.ID != reqE.ID || respE.Status != http.StatusTooManyRequests ||
		respE.Headers["Anthropic-Ratelimit-Unified-5h-Utilization"] != "0.018" {
		t.Errorf("response entry = %+v", respE)
	}
}
