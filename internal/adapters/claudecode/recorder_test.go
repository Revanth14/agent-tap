package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Revanth14/agent-tap/internal/telemetry"
)

// mainTurnSSE is a main-agent turn in the shape the Messages API streams it.
var mainTurnSSE = strings.Join([]string{
	`event: message_start`,
	`data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":1200,"cache_creation_input_tokens":3000,"cache_read_input_tokens":90000,"output_tokens":1}}}`,
	``,
	`event: ping`,
	`data: {"type":"ping"}`,
	``,
	`event: content_block_start`,
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	``,
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
	``,
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", world"}}`,
	``,
	`event: content_block_stop`,
	`data: {"type":"content_block_stop","index":0}`,
	``,
	`event: message_delta`,
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":42}}`,
	``,
	`event: message_stop`,
	`data: {"type":"message_stop"}`,
	``,
}, "\n") + "\n"

const mainTurnRequest = `{"model":"claude-opus-5-5","stream":true,` +
	`"system":[{"type":"text","text":"You are Claude Code."}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"what does store.go do?"}]}],` +
	`"tools":[{"name":"Read"},{"name":"Agent"}]}`

// recFixture is a recording proxy in front of a test upstream, with its hub
// served for WebSocket clients.
type recFixture struct {
	proxyURL, wsURL string
	store           *telemetry.Store
	rec             *Recorder
}

func newRecFixture(t *testing.T, upstream http.Handler) *recFixture {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	return newRecFixtureTo(t, up.URL)
}

func newRecFixtureTo(t *testing.T, upstreamURL string) *recFixture {
	t.Helper()
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	hub := telemetry.NewHub(store)
	rec := NewRecorder(store, hub)
	p, err := NewProxy(upstreamURL, rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(p)
	t.Cleanup(proxy.Close)
	mux := http.NewServeMux()
	hub.Register(mux)
	hubSrv := httptest.NewServer(mux)
	t.Cleanup(hubSrv.Close)
	return &recFixture{proxy.URL, "ws" + strings.TrimPrefix(hubSrv.URL, "http") + "/api/ws", store, rec}
}

// recordingProxy starts an upstream serving sse, and a recording proxy to it
// whose hub is reachable at the returned WebSocket URL.
func recordingProxy(t *testing.T, sse string) (proxyURL, wsURL string, store *telemetry.Store) {
	t.Helper()
	f := newRecFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	return f.proxyURL, f.wsURL, f.store
}

// scripted returns an upstream that answers successive requests with the
// given handlers, in order.
func scripted(t *testing.T, responses ...http.HandlerFunc) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if len(responses) == 0 {
			mu.Unlock()
			t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		next := responses[0]
		responses = responses[1:]
		mu.Unlock()
		next(w, r)
	})
}

func sseResponse(sse string, headers map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}
}

// sseTurn builds a streamed message from data payloads, one per event.
func sseTurn(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

// post sends a /v1/messages request with Claude Code's headers and drains the
// response, returning its status.
func post(t *testing.T, url string, headers map[string]string, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// waitFor polls the store until cond holds, failing after 5 seconds.
func waitFor(t *testing.T, store *telemetry.Store, cond func(telemetry.StateSnapshot) bool) telemetry.StateSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap := store.Snapshot()
		if cond(snap) {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; last snapshot: %+v", snap)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRecordsMainStreamingTurn(t *testing.T) {
	proxyURL, wsURL, store := recordingProxy(t, mainTurnSSE)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ev struct {
		Type telemetry.EventType `json:"type"`
	}
	if err := conn.ReadJSON(&ev); err != nil || ev.Type != telemetry.EventSnapshot {
		t.Fatalf("first event = %v, %v; want snapshot", ev.Type, err)
	}

	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages?beta=true", strings.NewReader(mainTurnRequest))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.296 (external, cli)")
	req.Header.Set("X-Claude-Code-Session-Id", "sess-1")
	req.Header.Set("X-Claude-Code-Request-Class", "main")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != mainTurnSSE {
		t.Errorf("client stream differs from upstream:\n%s", got)
	}

	snap := waitFor(t, store, func(s telemetry.StateSnapshot) bool {
		return len(s.Spans) == 1 && s.Spans[0].Status == "completed"
	})
	sp := snap.Spans[0]
	if sp.ID != "msg_01" || sp.SessionID != "sess-1" || sp.AgentID != "sess-1/main" ||
		sp.Role != telemetry.RoleMain || sp.Surface != telemetry.SurfaceTerminalCLI || sp.Model != "claude-opus-5-5" {
		t.Errorf("span identity = %+v", sp)
	}
	if sp.InputTokensUncached != 1200 || sp.InputTokensCacheWrite != 3000 || sp.InputTokensCacheRead != 90000 || sp.OutputTokens != 42 {
		t.Errorf("span tokens = %d/%d/%d/%d, want 1200/3000/90000/42",
			sp.InputTokensUncached, sp.InputTokensCacheWrite, sp.InputTokensCacheRead, sp.OutputTokens)
	}
	if want := 90000.0 / 94200.0; sp.CacheHitRatio != want {
		t.Errorf("CacheHitRatio = %v, want %v", sp.CacheHitRatio, want)
	}
	if sp.StopReason != "end_turn" || sp.AssistantOutputPreview != "Hello, world" ||
		sp.SystemPromptPreview != "You are Claude Code." || sp.LastUserMessagePreview != "what does store.go do?" ||
		!slices.Equal(sp.ToolsOffered, []string{"Read", "Agent"}) {
		t.Errorf("span content = %+v", sp)
	}
	if sp.FirstTokenAt == nil || sp.CompletedAt == nil || sp.TTFTMs < 0 || sp.DurationMs < sp.TTFTMs {
		t.Errorf("span timing = first %v, completed %v, ttft %d, duration %d", sp.FirstTokenAt, sp.CompletedAt, sp.TTFTMs, sp.DurationMs)
	}

	if len(snap.Sessions) != 1 {
		t.Fatalf("sessions = %+v", snap.Sessions)
	}
	if s := snap.Sessions[0]; s.ID != "sess-1" || s.Provider != "claude_code" || s.PrimarySurface != telemetry.SurfaceTerminalCLI ||
		s.TotalInputTokens != 1200 || s.TotalCacheWrite != 3000 || s.TotalCacheRead != 90000 || s.TotalOutputTokens != 42 ||
		s.CurrentContextSize != 94242 || s.MaxContextWindow != 1_000_000 {
		t.Errorf("session = %+v", s)
	}
	if len(snap.Agents) != 1 {
		t.Fatalf("agents = %+v", snap.Agents)
	}
	if a := snap.Agents[0]; a.ID != "sess-1/main" || a.Role != telemetry.RoleMain || a.ParentAgentID != "" ||
		a.Model != "claude-opus-5-5" || a.Status != "idle" || a.TotalTokens != 94242 {
		t.Errorf("agent = %+v", a)
	}

	// Events arrive in order: started before any delta, completed last.
	var types []telemetry.EventType
	for !slices.Contains(types, telemetry.EventSpanCompleted) {
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("after %v: %v", types, err)
		}
		types = append(types, ev.Type)
	}
	started := slices.Index(types, telemetry.EventSpanStarted)
	if started < 0 || slices.Index(types, telemetry.EventSessionUpdated) < 0 || slices.Index(types, telemetry.EventAgentUpserted) < 0 {
		t.Errorf("events = %v, want session.updated, agent.upserted, span.started", types)
	}
	if d := slices.Index(types, telemetry.EventSpanDelta); d >= 0 && d < started {
		t.Errorf("events = %v: span.delta before span.started", types)
	}
	// The span is written last, so whoever sees it completed sees the agent
	// and session it updated.
	if n := len(types); n < 3 || types[n-3] != telemetry.EventAgentUpserted || types[n-2] != telemetry.EventSessionUpdated {
		t.Errorf("events = %v, want agent.upserted, session.updated, then span.completed", types)
	}
}

func TestTokensPerSecCoversTheWholeRequest(t *testing.T) {
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(store, nil)
	tests := []struct {
		name             string
		ttft, elapsed    time.Duration
		output           int
		wantMin, wantMax float64
	}{
		// Real traffic: 889 tokens 1.2s after a 7.1s first token, once
		// reported as 723 tok/s; ~107 over the whole request.
		{"burst after a long first token", 7082 * time.Millisecond, 8312 * time.Millisecond, 889, 100, 115},
		{"burst a millisecond after message_start", 2165 * time.Millisecond, 2166 * time.Millisecond, 157, 65, 80},
		{"normal decode", 700 * time.Millisecond, 20 * time.Second, 2600, 125, 135},
		{"too short to measure", 0, 50 * time.Millisecond, 10, 0, 0},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &telemetry.InferenceSpan{
				ID:           fmt.Sprintf("s%d", i),
				AgentID:      "a",
				StartedAt:    time.Now().Add(-tt.elapsed),
				TTFTMs:       tt.ttft.Milliseconds(),
				OutputTokens: tt.output,
			}
			(&turn{rec: rec, span: sp}).complete()
			if sp.TokensPerSec < tt.wantMin || sp.TokensPerSec > tt.wantMax {
				t.Errorf("TokensPerSec = %v, want between %v and %v", sp.TokensPerSec, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestRecordsStreamCutShortAsError(t *testing.T) {
	cut := mainTurnSSE[:strings.Index(mainTurnSSE, "event: message_delta")]
	proxyURL, _, store := recordingProxy(t, cut)
	req, _ := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages", strings.NewReader(mainTurnRequest))
	req.Header.Set("X-Claude-Code-Session-Id", "sess-1")
	req.Header.Set("X-Claude-Code-Request-Class", "main")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	snap := waitFor(t, store, func(s telemetry.StateSnapshot) bool {
		return len(s.Spans) == 1 && s.Spans[0].CompletedAt != nil
	})
	if sp := snap.Spans[0]; sp.Status != "error" {
		t.Errorf("span status = %q, want error for a stream without message_stop", sp.Status)
	}
	if a := snap.Agents[0]; a.Status != "error" {
		t.Errorf("agent status = %q, want error", a.Status)
	}
}

var mainHeaders = map[string]string{
	"X-Claude-Code-Session-Id":    "sess-1",
	"X-Claude-Code-Request-Class": "main",
	"User-Agent":                  "claude-cli/2.1.296 (external, cli)",
}

func TestRecordsPlanUsageAndHTTPErrors(t *testing.T) {
	f := newRecFixture(t, scripted(t,
		sseResponse(mainTurnSSE, map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.42",
			"Anthropic-Ratelimit-Unified-5h-Reset":       "1792005000",
			"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.12",
			"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
		}),
		// A throttle: 429 without usage-limit headers.
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Request-Id", "req_429")
			w.Header().Set("X-Should-Retry", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error"}}`)
		},
	))
	post(t, f.proxyURL, mainHeaders, mainTurnRequest)
	if got := post(t, f.proxyURL, mainHeaders, mainTurnRequest); got != http.StatusTooManyRequests {
		t.Fatalf("client status = %d, want the upstream 429 passed through", got)
	}

	snap := waitFor(t, f.store, func(s telemetry.StateSnapshot) bool { return len(s.Spans) == 2 && s.Spans[1].CompletedAt != nil })
	u := snap.PlanUsage
	if u == nil || len(u.Windows) != 2 {
		t.Fatalf("PlanUsage = %+v, want 5h and 7d windows kept through the throttling 429", u)
	}
	if w := u.Windows[0]; w.Name != "5h" || w.Utilization != 0.42 || w.Status != "allowed" || w.ResetsAt == nil || !w.ResetsAt.Equal(time.Unix(1792005000, 0)) {
		t.Errorf("5h window = %+v", w)
	}
	if w := u.Windows[1]; w.Name != "7d" || w.Utilization != 0.12 || w.ResetsAt != nil {
		t.Errorf("7d window = %+v", w)
	}

	errSpan := snap.Spans[1]
	if errSpan.ID != "req_429" || errSpan.Status != "error" || errSpan.StopReason != "http_429" || errSpan.AgentID != "sess-1/main" {
		t.Errorf("error span = %+v", errSpan)
	}
	// The raw payload is filed once the response has been read to the end,
	// just after the span is recorded.
	var rr *httptest.ResponseRecorder
	for deadline := time.Now().Add(5 * time.Second); ; {
		rr = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/spans/req_429/raw", nil)
		req.SetPathValue("id", "req_429")
		f.rec.RawHandler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var raw struct{ Request, Response string }
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &raw) != nil ||
		raw.Request != mainTurnRequest || !strings.Contains(raw.Response, "rate_limit_error") {
		t.Errorf("raw payload = %d %s", rr.Code, rr.Body)
	}
}

// The rise a response reveals is filed under that response's session and
// span, after the span is stored so that it counts as in flight.
func TestAttributesPlanUsageToTheRevealingResponse(t *testing.T) {
	usage := func(util, status string) map[string]string {
		return map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": util,
			"Anthropic-Ratelimit-Unified-5h-Reset":       "1792005000",
			"Anthropic-Ratelimit-Unified-5h-Status":      status,
		}
	}
	turnWithID := func(id string) string {
		return sseTurn(
			`{"type":"message_start","message":{"id":"`+id+`","model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":1}}}`,
			`{"type":"message_stop"}`,
		)
	}
	f := newRecFixture(t, scripted(t,
		sseResponse(turnWithID("msg_a"), usage("0.42", "allowed")),
		sseResponse(turnWithID("msg_b"), usage("0.43", "allowed")),
		func(w http.ResponseWriter, r *http.Request) { // The plan limit itself
			for k, v := range usage("1.0", "rejected") {
				w.Header().Set(k, v)
			}
			w.Header().Set("Request-Id", "req_limit")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error"}}`)
		},
	))
	for range 3 {
		post(t, f.proxyURL, mainHeaders, mainTurnRequest)
	}
	waitFor(t, f.store, func(telemetry.StateSnapshot) bool { return len(f.store.UsageHistory("5h")) == 3 })

	h := f.store.UsageHistory("5h")
	if got := h[0]; got.SessionID != "sess-1" || got.SpanID != "msg_a" || got.Bucket != telemetry.BucketUnseen {
		t.Errorf("first value = %+v, want from sess-1's msg_a, Unseen (nothing came before)", got)
	}
	if got := h[1]; got.SpanID != "msg_b" || got.Rise != 0.01 || got.Bucket != telemetry.BucketAttributed || strings.Join(got.Sessions, ",") != "sess-1" {
		t.Errorf("rise = %+v, want 0.01 from msg_b, Attributed to sess-1", got)
	}
	if got := h[2]; got.SpanID != "req_limit" || got.Status != "rejected" || got.Bucket != telemetry.BucketAttributed {
		t.Errorf("limit = %+v, want rejected from req_limit, Attributed", got)
	}
}

func TestRecordsUnreachableUpstream(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	f := newRecFixtureTo(t, url)
	if got := post(t, f.proxyURL, mainHeaders, mainTurnRequest); got != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", got)
	}
	snap := waitFor(t, f.store, func(s telemetry.StateSnapshot) bool { return len(s.Spans) == 1 })
	if sp := snap.Spans[0]; sp.Status != "error" || sp.StopReason != "upstream_unreachable" {
		t.Errorf("span = %+v, want an upstream_unreachable error", sp)
	}
}

func TestRecordsClientCancelMidStream(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f := newRecFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, mainTurnSSE[:strings.Index(mainTurnSSE, "event: content_block_delta")])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.proxyURL+"/v1/messages", strings.NewReader(mainTurnRequest))
	for k, v := range mainHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, f.store, func(s telemetry.StateSnapshot) bool { return len(s.Spans) == 1 })
	cancel() // Esc in Claude Code
	resp.Body.Close()

	snap := waitFor(t, f.store, func(s telemetry.StateSnapshot) bool { return len(s.Spans) == 1 && s.Spans[0].CompletedAt != nil })
	if sp := snap.Spans[0]; sp.Status != "error" || sp.StopReason != "client_canceled" {
		t.Errorf("span = %+v, want a client_canceled error", sp)
	}
}

// TestRecordsToolsAndAsyncSubagent follows the order real traffic showed:
// the Agent call returns at once ("Async agent launched"), the main agent's
// next request carries the results followed by a role "system" message, and
// only then does the subagent send its first request.
func TestRecordsToolsAndAsyncSubagent(t *testing.T) {
	const taskPrompt = "Find where the ring buffer is"
	mainWithTools := sseTurn(
		`{"type":"message_start","message":{"id":"msg_main1","model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_read_input_tokens":5000}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_agent","name":"Agent","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"description\":\"Find ring\","}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"prompt\":\"`+taskPrompt+`\",\"subagent_type\":\"Explore\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_read","name":"Read","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/repo/store.go\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":50}}`,
		`{"type":"message_stop"}`,
	)
	textTurn := func(id string) string {
		return sseTurn(
			`{"type":"message_start","message":{"id":"`+id+`","model":"claude-opus-5-5","usage":{"input_tokens":10}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
			`{"type":"message_stop"}`,
		)
	}
	f := newRecFixture(t, scripted(t,
		sseResponse(mainWithTools, nil),
		sseResponse(textTurn("msg_main2"), nil),
		sseResponse(textTurn("msg_sub1"), nil),
	))

	post(t, f.proxyURL, mainHeaders, mainTurnRequest)
	snap := waitFor(t, f.store, func(s telemetry.StateSnapshot) bool {
		return len(s.Spans) == 1 && s.Spans[0].CompletedAt != nil
	})
	tools := map[string]telemetry.ToolInvocation{}
	for _, tl := range snap.Tools {
		tools[tl.ToolUseID] = tl
	}
	if tl := tools["toolu_agent"]; tl.ToolName != "Agent" || tl.Target != "Find ring" || tl.SpanID != "msg_main1" || tl.AgentID != "sess-1/main" {
		t.Errorf("Agent tool = %+v", tl)
	}
	if tl := tools["toolu_read"]; tl.Target != "/repo/store.go" {
		t.Errorf("Read tool = %+v", tl)
	}
	if a := snap.Agents[0]; a.Status != "running_tool" || a.ActiveTool != "Read: /repo/store.go" {
		t.Errorf("main agent = %+v, want running_tool on the last tool", a)
	}
	if sp := snap.Spans[0]; len(sp.ToolsInvoked) != 2 {
		t.Errorf("span ToolsInvoked = %+v, want both tools", sp.ToolsInvoked)
	}

	post(t, f.proxyURL, mainHeaders, `{"model":"claude-opus-5-5","messages":[`+
		`{"role":"user","content":"hi"},`+
		`{"role":"system","content":"context"},`+
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_agent","name":"Agent","input":{}},{"type":"tool_use","id":"toolu_read","name":"Read","input":{}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_agent","content":"Async agent launched successfully."},`+
		`{"type":"tool_result","tool_use_id":"toolu_read","is_error":true,"content":"denied"}]},`+
		`{"role":"system","content":[{"type":"text","text":"reminder"}]}],"tools":[{"name":"Agent"}]}`)
	snap = waitFor(t, f.store, func(s telemetry.StateSnapshot) bool { return len(s.Spans) == 2 && s.Spans[1].CompletedAt != nil })
	tools = map[string]telemetry.ToolInvocation{}
	for _, tl := range snap.Tools {
		tools[tl.ToolUseID] = tl
	}
	if tools["toolu_agent"].Status != "completed" || tools["toolu_read"].Status != "failed" || tools["toolu_agent"].DurationMs < 0 {
		t.Errorf("tools after results behind a trailing system message = %+v", tools)
	}
	if a := snap.Agents[0]; a.Status != "idle" || a.ActiveTool != "" {
		t.Errorf("main agent = %+v, want idle with no active tool", a)
	}

	post(t, f.proxyURL, map[string]string{
		"X-Claude-Code-Session-Id":    "sess-1",
		"X-Claude-Code-Request-Class": "subagent",
		"X-Claude-Code-Agent-Id":      "a1",
		"X-Claude-Code-Agent-Type":    "Explore",
	}, `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"`+taskPrompt+`\n\nReport file paths."}],"tools":[{"name":"Read"}]}`)
	snap = waitFor(t, f.store, func(s telemetry.StateSnapshot) bool {
		return len(s.Agents) == 2 && len(s.Spans) == 3 && s.Spans[2].CompletedAt != nil
	})
	var sub telemetry.AgentNode
	for _, a := range snap.Agents {
		if a.ID == "a1" {
			sub = a
		}
	}
	if sub.ParentToolID != "toolu_agent" || sub.ParentAgentID != "sess-1/main" || sub.SubagentType != "Explore" ||
		sub.Label != taskPrompt+" Report file paths." {
		t.Errorf("subagent = %+v, want linked to the already-returned Agent call and labelled with its task", sub)
	}
	if sub.Status != "idle" || sub.CompletedAt != nil {
		t.Errorf("subagent = %+v, want idle: the launch acknowledgment does not mean it finished", sub)
	}
}

// TestLateParserEventsDoNotRegressTools covers Claude Code sending its next
// request before the parser has handled the previous response's events.
func TestLateParserEventsDoNotRegressTools(t *testing.T) {
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(store, nil)
	info := requestInfo{SessionID: "s", AgentID: "s/main", AgentRole: telemetry.RoleMain, Role: telemetry.RoleMain}
	sp := &telemetry.InferenceSpan{ID: "m1", SessionID: "s", AgentID: "s/main", Role: telemetry.RoleMain}
	rec.spanStarted(sp, info)
	rec.toolStarted(sp, info, "t1", "Read")
	rec.toolArgsDone(sp, info, "t1", "/a.go", "")

	next := requestInfo{SessionID: "s", AgentID: "s/main", ToolResults: []toolResult{{ToolUseID: "t1"}, {ToolUseID: "t2"}}}
	rec.requestArrived(next, time.Now()) // t2's own events have not been parsed yet

	rec.toolStarted(sp, info, "t2", "Grep")
	rec.toolArgsDone(sp, info, "t2", "ring", "")
	rec.spanCompleted(sp, info, []string{"t1", "t2"})

	snap := store.Snapshot()
	for _, tl := range snap.Tools {
		if tl.Status != "completed" {
			t.Errorf("tool %s = %q, want completed: a late event moved it back", tl.ToolUseID, tl.Status)
		}
	}
	if len(snap.Tools) != 2 || snap.Tools[1].ToolName == "" && snap.Tools[0].ToolName == "" {
		t.Errorf("tools = %+v, want both, with names", snap.Tools)
	}
	if a := snap.Agents[0]; a.Status != "idle" || a.ActiveTool != "" {
		t.Errorf("agent = %+v, want idle with no active tool", a)
	}
}

func TestRecordsNonStreamingResponse(t *testing.T) {
	f := newRecFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_json","type":"message","model":"claude-haiku-5-5","stop_reason":"tool_use",`+
			`"content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_g","name":"Grep","input":{"pattern":"ring"}}],`+
			`"usage":{"input_tokens":30,"cache_read_input_tokens":70,"output_tokens":12}}`)
	}))
	post(t, f.proxyURL, mainHeaders, mainTurnRequest)
	snap := waitFor(t, f.store, func(s telemetry.StateSnapshot) bool { return len(s.Spans) == 1 && s.Spans[0].CompletedAt != nil })
	sp := snap.Spans[0]
	if sp.ID != "msg_json" || sp.Status != "completed" || sp.Model != "claude-haiku-5-5" || sp.OutputTokens != 12 ||
		sp.InputTokensCacheRead != 70 || sp.StopReason != "tool_use" || sp.AssistantOutputPreview != "Checking." {
		t.Errorf("span = %+v", sp)
	}
	if len(snap.Tools) != 1 || snap.Tools[0].Target != "ring" || snap.Tools[0].Status != "running" {
		t.Errorf("tools = %+v", snap.Tools)
	}
}

func TestRawBufferKeepsNewestWithinBudget(t *testing.T) {
	b := newRawBuffer(100)
	for _, id := range []string{"a", "b", "c"} {
		b.put(id, &rawPayload{Request: make([]byte, 30), Response: make([]byte, 10)})
	}
	if _, ok := b.get("a"); ok {
		t.Error("a still kept; want the oldest dropped once 120 bytes exceed the 100-byte budget")
	}
	for _, id := range []string{"b", "c"} {
		if _, ok := b.get(id); !ok {
			t.Errorf("%s dropped; want the newest kept", id)
		}
	}

	resp := &cappedBuffer{limit: rawPayloadMax}
	resp.Write(make([]byte, rawPayloadMax+1))
	p := newRawPayload(make([]byte, rawPayloadMax+1), resp)
	if !p.RequestTooLarge || !p.ResponseTooLarge || p.size() != 0 {
		t.Errorf("oversized payload = %d bytes, flags %v %v; want nothing kept and both flagged", p.size(), p.RequestTooLarge, p.ResponseTooLarge)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/spans/gone/raw", nil)
	req.SetPathValue("id", "gone")
	NewRecorder(nil, nil).RawHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown span: status %d, want 404", rr.Code)
	}
}

func TestRecorderStateIsBounded(t *testing.T) {
	rec := NewRecorder(nil, nil)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i := range telemetry.MaxLiveSessions + 5 {
		rec.state(fmt.Sprintf("s%d", i))
	}
	if len(rec.live) != telemetry.MaxLiveSessions {
		t.Errorf("live sessions = %d, want %d", len(rec.live), telemetry.MaxLiveSessions)
	}
	if _, ok := rec.live["s0"]; ok {
		t.Error("s0 kept; want the least recently written session dropped")
	}
}

// TestRestoreContinuesSessionTotals covers the first turn after a daemon
// restart: it must add to the restored session, not start a new record.
func TestRestoreContinuesSessionTotals(t *testing.T) {
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)
	restored := telemetry.StateSnapshot{
		Sessions: []telemetry.Session{{ID: "s", Provider: "claude_code", WorkspacePath: "/repo", PrimarySurface: telemetry.SurfaceVSCodeExt,
			StartedAt: t0, TotalOutputTokens: 1000, TotalCacheRead: 500_000, MaxContextWindow: 1_000_000}},
		Agents: []telemetry.AgentNode{{ID: "s/main", SessionID: "s", Role: telemetry.RoleMain, StartedAt: t0, TotalTokens: 501_000}},
		Spans:  []telemetry.InferenceSpan{{ID: "old", SessionID: "s", AgentID: "s/main", Role: telemetry.RoleMain, StartedAt: t0, InputTokensCacheRead: 500_000}},
		Tools:  []telemetry.ToolInvocation{{ToolUseID: "t-run", SpanID: "old", AgentID: "s/main", ToolName: "Bash", Status: "running", StartedAt: t0}},
	}
	for _, a := range restored.Agents {
		store.UpsertAgent(a)
	}
	rec := NewRecorder(store, nil)
	rec.Restore(restored)

	info := requestInfo{SessionID: "s", AgentID: "s/main", AgentRole: telemetry.RoleMain, Role: telemetry.RoleMain,
		Surface: telemetry.SurfaceVSCodeExt, ToolResults: []toolResult{{ToolUseID: "t-run"}}}
	rec.requestArrived(info, time.Now())
	// A cache read under half the last main turn's, after the restart: still detected.
	sp := &telemetry.InferenceSpan{ID: "new", SessionID: "s", AgentID: "s/main", Role: telemetry.RoleMain, StartedAt: time.Now(),
		InputTokensUncached: 20_000, InputTokensCacheRead: 100_000, OutputTokens: 50}
	rec.spanStarted(sp, info)
	rec.spanCompleted(sp, info, nil)

	snap := store.Snapshot()
	var sess telemetry.Session
	for _, x := range snap.Sessions {
		if x.ID == "s" {
			sess = x
		}
	}
	if sess.TotalOutputTokens != 1050 || sess.TotalCacheRead != 600_000 || sess.WorkspacePath != "/repo" || !sess.StartedAt.Equal(t0) {
		t.Errorf("session = %+v, want restored totals continued", sess)
	}
	if !sp.CacheInvalidated {
		t.Error("CacheInvalidated = false, want the drop against the restored turn detected")
	}
	for _, a := range snap.Agents {
		if a.ID == "s/main" && a.TotalTokens != 501_000+120_050 {
			t.Errorf("agent TotalTokens = %d, want restored total continued", a.TotalTokens)
		}
	}
	for _, tl := range snap.Tools {
		if tl.ToolUseID == "t-run" && tl.Status != "completed" {
			t.Errorf("restored running tool = %q, want completed by the new request's tool_result", tl.Status)
		}
	}
}
