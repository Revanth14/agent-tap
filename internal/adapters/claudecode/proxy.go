// Package claudecode adapts Claude Code traffic and hooks to the canonical
// telemetry schema.
package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Proxy forwards Claude Code API traffic to the Anthropic upstream. It never
// changes request bodies: re-encoding them would invalidate Anthropic's
// prompt-cache prefix.
type Proxy struct {
	rp   *httputil.ReverseProxy
	rec  *Recorder // nil: forward without recording
	wire *wireLog  // nil when the wire log is off
}

// NewProxy returns a proxy to upstream, e.g. https://api.anthropic.com. If rec
// is non-nil, POST /v1/messages traffic is recorded to it. If wireOut is
// non-nil, each request and response is logged to it as a JSON line; see
// wireEntry for what is logged.
func NewProxy(upstream string, rec *Recorder, wireOut io.Writer) (*Proxy, error) {
	target, err := url.Parse(upstream)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("claudecode: invalid upstream URL %q", upstream)
	}
	p := &Proxy{rec: rec}
	if wireOut != nil {
		p.wire = &wireLog{w: wireOut}
		if rec != nil {
			rec.wire = p.wire
		}
	}
	p.rp = &httputil.ReverseProxy{
		// Rewrite has already dropped hop-by-hop headers from pr.Out; every
		// other header is forwarded as received.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The tap reads the response stream, so it must arrive
			// uncompressed. This is the only request header the proxy changes.
			pr.Out.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: func(resp *http.Response) error {
			if p.wire != nil {
				p.wire.response(resp)
			}
			if t, ok := resp.Request.Context().Value(turnKey{}).(*turn); ok {
				t.response(resp)
			} else if p.rec != nil {
				p.rec.ObserveUsage(resp.Header)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if t, ok := r.Context().Value(turnKey{}).(*turn); ok {
				t.unreachable(err)
			}
			log.Printf("atap: proxy error: %v", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return p, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Buffer the body so it can be inspected, then forward the same bytes.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "atap: read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	if p.wire != nil {
		r = r.WithContext(context.WithValue(r.Context(), wireIDKey{}, p.wire.request(r, body)))
	}
	if p.rec != nil && r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
		r = r.WithContext(context.WithValue(r.Context(), turnKey{}, p.rec.begin(r.Header.Clone(), body)))
	}
	p.rp.ServeHTTP(w, r)
}

type turnKey struct{}

// wireLog writes request and response metadata as JSON lines, for checking
// what Claude Code actually sends. Header values are logged only for
// allowlisted names, so credentials (Authorization, x-api-key, Cookie) can
// never leak by omission; bodies are never logged.
type wireLog struct {
	mu   sync.Mutex
	w    io.Writer
	next uint64
}

type wireIDKey struct{}

type wireEntry struct {
	ID          uint64            `json:"id"` // Pairs a request with its response
	Time        time.Time         `json:"time"`
	Dir         string            `json:"dir"` // "request", "response" or "hook"
	Method      string            `json:"method,omitempty"`
	Path        string            `json:"path"` // Without the query string
	Status      int               `json:"status,omitempty"`
	HeaderNames []string          `json:"header_names"`
	Headers     map[string]string `json:"headers,omitempty"` // Allowlisted values only
	Model       string            `json:"model,omitempty"`   // From a /v1/messages request body

	// Hooks: the event and the key names of the payload, never values.
	Event            string   `json:"event,omitempty"`
	Keys             []string `json:"keys,omitempty"`
	ToolResponseKeys []string `json:"tool_response_keys,omitempty"`
}

func (l *wireLog) request(r *http.Request, body []byte) uint64 {
	l.mu.Lock()
	l.next++
	id := l.next
	l.mu.Unlock()

	e := wireEntry{ID: id, Dir: "request", Method: r.Method, Path: r.URL.Path}
	e.HeaderNames, e.Headers = describeHeaders(r.Header)
	if r.URL.Path == "/v1/messages" {
		var m struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &m) == nil {
			e.Model = m.Model
		}
	}
	l.write(e)
	return id
}

func (l *wireLog) response(resp *http.Response) {
	id, _ := resp.Request.Context().Value(wireIDKey{}).(uint64)
	e := wireEntry{ID: id, Dir: "response", Path: resp.Request.URL.Path, Status: resp.StatusCode}
	e.HeaderNames, e.Headers = describeHeaders(resp.Header)
	l.write(e)
}

func (l *wireLog) hook(event string, body []byte) {
	l.mu.Lock()
	l.next++
	id := l.next
	l.mu.Unlock()
	e := wireEntry{ID: id, Dir: "hook", Path: "/api/hooks/claude", Event: event}
	e.Keys, e.ToolResponseKeys = hookKeys(body)
	l.write(e)
}

func (l *wireLog) write(e wireEntry) {
	e.Time = time.Now().UTC()
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Write(append(line, '\n'))
}

// describeHeaders returns every header name, sorted, and the values of the
// allowlisted ones.
func describeHeaders(h http.Header) (names []string, values map[string]string) {
	values = make(map[string]string)
	for name, vs := range h {
		names = append(names, name)
		if loggableHeader(name) {
			values[name] = strings.Join(vs, ", ")
		}
	}
	slices.Sort(names)
	return names, values
}

func loggableHeader(name string) bool {
	n := strings.ToLower(name)
	return n == "user-agent" ||
		n == "anthropic-beta" ||
		strings.HasPrefix(n, "x-claude-code-") ||
		strings.HasPrefix(n, "anthropic-ratelimit-unified-")
}
