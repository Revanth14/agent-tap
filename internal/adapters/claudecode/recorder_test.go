package claudecode

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// recordingProxy starts an upstream serving sse, and a recording proxy to it
// whose hub is reachable at the returned WebSocket URL.
func recordingProxy(t *testing.T, sse string) (proxyURL, wsURL string, store *telemetry.Store) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	t.Cleanup(up.Close)

	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	hub := telemetry.NewHub(store)
	p, err := NewProxy(up.URL, NewRecorder(store, hub), nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(p)
	t.Cleanup(proxy.Close)
	mux := http.NewServeMux()
	hub.Register(mux)
	hubSrv := httptest.NewServer(mux)
	t.Cleanup(hubSrv.Close)
	return proxy.URL, "ws" + strings.TrimPrefix(hubSrv.URL, "http") + "/api/ws", store
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
}

func TestTokensPerSecNeedsAMeasurableDecodeWindow(t *testing.T) {
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(store, nil)
	tests := []struct {
		name          string
		ttft, elapsed time.Duration
		wantMin       float64
		wantMax       float64
	}{
		// Real traffic: message_start after 2165ms, then 157 tokens within 1ms.
		{"burst after a late message_start", 2165 * time.Millisecond, 2166 * time.Millisecond, 0, 0},
		{"normal decode", time.Second, 3 * time.Second, 150, 250}, // 400 tokens over ~2s
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &telemetry.InferenceSpan{
				ID:           fmt.Sprintf("s%d", i),
				AgentID:      "a",
				StartedAt:    time.Now().Add(-tt.elapsed),
				TTFTMs:       tt.ttft.Milliseconds(),
				OutputTokens: 400,
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
