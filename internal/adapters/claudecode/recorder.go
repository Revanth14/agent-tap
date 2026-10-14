package claudecode

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Revanth14/agent-tap/internal/telemetry"
)

const (
	provider              = "claude_code"
	assistantPreviewBytes = 4096
	deltaInterval         = 100 * time.Millisecond

	// minRateWindow is the shortest decode window with a meaningful
	// tokens-per-second figure. A response that arrives in one burst after
	// message_start (seen on real traffic: 157 tokens in 1ms) gets no rate.
	minRateWindow = 100 * time.Millisecond
)

// Recorder turns proxied /v1/messages traffic into canonical telemetry. Every
// change is written to the store before it is broadcast, as the hub requires.
type Recorder struct {
	store *telemetry.Store
	hub   *telemetry.Hub // nil: record without broadcasting
	seq   atomic.Uint64  // IDs for spans without a message ID

	mu                sync.Mutex // Guards the maps below
	sessions          map[string]*telemetry.Session
	agents            map[string]*telemetry.AgentNode
	lastMainCacheRead map[string]int // By session ID
}

// NewRecorder returns a recorder writing to store and broadcasting on hub,
// which may be nil.
func NewRecorder(store *telemetry.Store, hub *telemetry.Hub) *Recorder {
	return &Recorder{
		store:             store,
		hub:               hub,
		sessions:          make(map[string]*telemetry.Session),
		agents:            make(map[string]*telemetry.AgentNode),
		lastMainCacheRead: make(map[string]int),
	}
}

// turn records one /v1/messages request and its response.
type turn struct {
	rec       *Recorder
	startedAt time.Time
	ready     chan struct{} // Closed once info is classified
	info      requestInfo

	// Owned by the response goroutine.
	span      *telemetry.InferenceSpan
	lastDelta time.Time
}

// begin starts recording a request. Classification runs off the request
// path, so forwarding never waits on parsing; the response parser waits for
// it instead. h and body must not be modified afterwards.
func (rec *Recorder) begin(h http.Header, body []byte) *turn {
	t := &turn{rec: rec, startedAt: time.Now(), ready: make(chan struct{})}
	go func() {
		t.info = classify(h, body)
		close(t.ready)
	}()
	return t
}

// response taps a streaming response's body for the SSE parser. Error and
// non-streaming responses are not recorded yet.
func (t *turn) response(resp *http.Response) {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode/100 != 2 || mediaType != "text/event-stream" {
		return
	}
	tap := newStreamTap()
	resp.Body = &teeBody{ReadCloser: resp.Body, tap: tap}
	go t.consumeSSE(tap)
}

type sseUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type sseEvent struct {
	Type    string `json:"type"`
	Message struct {
		ID    string   `json:"id"`
		Model string   `json:"model"`
		Usage sseUsage `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage sseUsage `json:"usage"`
}

func (t *turn) consumeSSE(tap *streamTap) {
	defer tap.stop()
	<-t.ready
	sc := bufio.NewScanner(tap)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:"))
		if !ok {
			continue
		}
		var ev sseEvent
		if json.Unmarshal(bytes.TrimSpace(data), &ev) == nil {
			t.handle(ev)
		}
	}
	if t.span != nil && t.span.CompletedAt == nil {
		t.fail() // The stream ended before message_stop.
	}
}

func (t *turn) handle(ev sseEvent) {
	if ev.Type == "message_start" && t.span == nil {
		t.start(ev)
		return
	}
	if t.span == nil || t.span.CompletedAt != nil {
		return
	}
	switch ev.Type {
	case "content_block_delta":
		if ev.Delta.Type == "text_delta" {
			t.appendText(ev.Delta.Text)
		}
	case "message_delta":
		t.span.OutputTokens = ev.Usage.OutputTokens
		t.span.StopReason = ev.Delta.StopReason
	case "message_stop":
		t.complete()
	case "error":
		t.fail()
	}
}

func (t *turn) start(ev sseEvent) {
	now := time.Now()
	u := ev.Message.Usage
	sp := &telemetry.InferenceSpan{
		ID:                     ev.Message.ID,
		SessionID:              t.info.SessionID,
		AgentID:                t.info.AgentID,
		Surface:                t.info.Surface,
		Role:                   t.info.Role,
		Model:                  cmp.Or(ev.Message.Model, t.info.Model),
		Status:                 "decoding",
		StartedAt:              t.startedAt,
		FirstTokenAt:           &now,
		TTFTMs:                 now.Sub(t.startedAt).Milliseconds(),
		InputTokensUncached:    u.InputTokens,
		InputTokensCacheWrite:  u.CacheCreationInputTokens,
		InputTokensCacheRead:   u.CacheReadInputTokens,
		OutputTokens:           u.OutputTokens,
		ToolsOffered:           t.info.ToolsOffered,
		SystemPromptPreview:    t.info.SystemPreview,
		LastUserMessagePreview: t.info.LastUserPreview,
	}
	if in := inputTokens(sp); in > 0 {
		sp.CacheHitRatio = float64(sp.InputTokensCacheRead) / float64(in)
	}
	if sp.ID == "" {
		sp.ID = fmt.Sprintf("atap-%d", t.rec.seq.Add(1))
	}
	t.span = sp
	t.rec.spanStarted(sp, t.info)
}

func (t *turn) appendText(s string) {
	sp := t.span
	if room := assistantPreviewBytes - len(sp.AssistantOutputPreview); room > 0 {
		sp.AssistantOutputPreview += cutBytes(s, room)
	}
	if now := time.Now(); now.Sub(t.lastDelta) >= deltaInterval {
		t.lastDelta = now
		t.rec.spanDelta(sp)
	}
}

func (t *turn) complete() {
	now := time.Now()
	sp := t.span
	sp.Status, sp.CompletedAt = "completed", &now
	sp.DurationMs = now.Sub(sp.StartedAt).Milliseconds()
	if decodeMs := sp.DurationMs - sp.TTFTMs; decodeMs >= minRateWindow.Milliseconds() {
		sp.TokensPerSec = float64(sp.OutputTokens) / (float64(decodeMs) / 1000)
	}
	t.rec.spanCompleted(sp, t.info)
}

// fail ends a span whose stream reported an error or broke off.
func (t *turn) fail() {
	now := time.Now()
	sp := t.span
	sp.Status, sp.CompletedAt = "error", &now
	sp.DurationMs = now.Sub(sp.StartedAt).Milliseconds()
	t.rec.spanFailed(sp, t.info)
}

func (rec *Recorder) spanStarted(sp *telemetry.InferenceSpan, info requestInfo) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if sp.Role == telemetry.RoleMain {
		prev, seen := rec.lastMainCacheRead[sp.SessionID]
		sp.CacheInvalidated = seen && sp.InputTokensCacheRead*2 < prev &&
			sp.InputTokensUncached+sp.InputTokensCacheWrite > 10_000
		rec.lastMainCacheRead[sp.SessionID] = sp.InputTokensCacheRead
	}
	sess, created := rec.session(sp, info)
	agent := rec.agent(sp, info)
	agent.Status = "streaming"

	if created {
		rec.check(rec.store.UpsertSession(*sess))
		rec.broadcast(telemetry.EventSessionUpdated, *sess)
	}
	rec.check(rec.store.UpsertAgent(*agent))
	rec.check(rec.store.UpsertSpan(*sp))
	rec.broadcast(telemetry.EventAgentUpserted, *agent)
	rec.broadcast(telemetry.EventSpanStarted, *sp)
}

func (rec *Recorder) spanDelta(sp *telemetry.InferenceSpan) {
	rec.check(rec.store.UpsertSpan(*sp))
	rec.broadcast(telemetry.EventSpanDelta, *sp)
}

func (rec *Recorder) spanCompleted(sp *telemetry.InferenceSpan, info requestInfo) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	agent := rec.agent(sp, info)
	agent.TotalTokens += inputTokens(sp) + sp.OutputTokens
	agent.Status = "idle"
	rec.check(rec.store.UpsertSpan(*sp))
	rec.check(rec.store.UpsertAgent(*agent))
	rec.broadcast(telemetry.EventSpanCompleted, *sp)
	rec.broadcast(telemetry.EventAgentUpserted, *agent)

	sess, _ := rec.session(sp, info)
	if sess == nil {
		return
	}
	// TotalInputTokens counts uncached input only; cache reads and writes
	// have their own totals.
	sess.TotalInputTokens += sp.InputTokensUncached
	sess.TotalCacheWrite += sp.InputTokensCacheWrite
	sess.TotalCacheRead += sp.InputTokensCacheRead
	sess.TotalOutputTokens += sp.OutputTokens
	if sp.Role == telemetry.RoleMain {
		sess.CurrentContextSize = inputTokens(sp) + sp.OutputTokens
		sess.MaxContextWindow = telemetry.MaxContextWindow(sp.Model, info.LongContext)
	}
	sess.UpdatedAt = time.Now()
	rec.check(rec.store.UpsertSession(*sess))
	rec.broadcast(telemetry.EventSessionUpdated, *sess)
}

func (rec *Recorder) spanFailed(sp *telemetry.InferenceSpan, info requestInfo) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	agent := rec.agent(sp, info)
	agent.Status = "error"
	rec.check(rec.store.UpsertSpan(*sp))
	rec.check(rec.store.UpsertAgent(*agent))
	rec.broadcast(telemetry.EventSpanCompleted, *sp)
	rec.broadcast(telemetry.EventAgentUpserted, *agent)
}

// session returns the span's session, creating it on first sight; nil when the
// request carried no session ID. Callers hold rec.mu.
func (rec *Recorder) session(sp *telemetry.InferenceSpan, info requestInfo) (sess *telemetry.Session, created bool) {
	if sp.SessionID == "" {
		return nil, false
	}
	sess, ok := rec.sessions[sp.SessionID]
	if !ok {
		sess = &telemetry.Session{
			ID:               sp.SessionID,
			Provider:         provider,
			StartedAt:        sp.StartedAt,
			UpdatedAt:        time.Now(),
			MaxContextWindow: telemetry.DefaultMaxContextWindow,
		}
		rec.sessions[sp.SessionID] = sess
	}
	if sess.PrimarySurface == "" || sess.PrimarySurface == telemetry.SurfaceUnknown {
		sess.PrimarySurface = info.Surface
	}
	return sess, !ok
}

// agent returns the span's agent node, creating it on first sight. A helper
// call made on an agent's behalf does not relabel the agent's model. Callers
// hold rec.mu.
func (rec *Recorder) agent(sp *telemetry.InferenceSpan, info requestInfo) *telemetry.AgentNode {
	a, ok := rec.agents[sp.AgentID]
	if !ok {
		a = &telemetry.AgentNode{
			ID:            sp.AgentID,
			SessionID:     sp.SessionID,
			ParentAgentID: info.ParentAgentID,
			Role:          info.AgentRole,
			SubagentType:  info.SubagentType,
			StartedAt:     sp.StartedAt,
		}
		rec.agents[a.ID] = a
	}
	a.Surface = sp.Surface
	if a.Model == "" || sp.Role == a.Role {
		a.Model = sp.Model
	}
	return a
}

func (rec *Recorder) broadcast(eventType telemetry.EventType, payload any) {
	if rec.hub != nil {
		rec.check(rec.hub.Broadcast(eventType, payload))
	}
}

func (rec *Recorder) check(err error) {
	if err != nil {
		log.Printf("atap: record telemetry: %v", err)
	}
}

func inputTokens(sp *telemetry.InferenceSpan) int {
	return sp.InputTokensUncached + sp.InputTokensCacheWrite + sp.InputTokensCacheRead
}

// cutBytes returns at most n bytes of s, cut on a rune boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
