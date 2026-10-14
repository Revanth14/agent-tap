package claudecode

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
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
	targetRunes           = 200
	labelRunes            = 80

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
	raw   *rawBuffer
	seq   atomic.Uint64 // IDs for spans with no message or request ID

	mu     sync.Mutex // Guards live and everything reachable from it
	live   map[string]*sessionState
	writes uint64 // Write counter, for least-recently-written eviction
}

// sessionState is the recorder's working state for one session. Like the
// store, the recorder keeps at most telemetry.MaxLiveSessions of them.
type sessionState struct {
	session       *telemetry.Session // nil for requests without a session ID
	agents        map[string]*telemetry.AgentNode
	tools         map[string]*toolState // By tool_use ID
	mainCacheRead int
	seenMain      bool
	writtenAt     uint64
}

type toolState struct {
	inv        telemetry.ToolInvocation
	prompt     string    // Full Task/Agent prompt, for subagent linking
	linked     bool      // A subagent has been linked to this Task/Agent call
	argsDoneAt time.Time // When the call's arguments finished streaming
	runningAt  time.Time // When Claude Code began executing the tool
	seen       bool      // false while only its result has arrived
}

// finished reports whether the tool's result has arrived. Parser events for
// a response can trail the next request, so later updates must not move a
// finished tool back.
func (ts *toolState) finished() bool {
	return ts.inv.Status == "completed" || ts.inv.Status == "failed"
}

// NewRecorder returns a recorder writing to store and broadcasting on hub,
// which may be nil.
func NewRecorder(store *telemetry.Store, hub *telemetry.Hub) *Recorder {
	return &Recorder{
		store: store,
		hub:   hub,
		raw:   newRawBuffer(rawBufferMax),
		live:  make(map[string]*sessionState),
	}
}

// ObserveUsage records the plan usage-limit headers of any upstream
// response. A response without them, such as a throttling 429, leaves the
// last value in place.
func (rec *Recorder) ObserveUsage(h http.Header) {
	u, ok := planUsage(h, time.Now().UTC())
	if !ok {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.store.SetPlanUsage(u)
	rec.broadcast(telemetry.EventUsageUpdated, u)
}

func planUsage(h http.Header, now time.Time) (telemetry.PlanUsage, bool) {
	var windows []telemetry.UsageWindow
	for _, name := range []string{"5h", "7d"} {
		prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
		util, status, reset := h.Get(prefix+"Utilization"), h.Get(prefix+"Status"), h.Get(prefix+"Reset")
		if util == "" && status == "" && reset == "" {
			continue
		}
		w := telemetry.UsageWindow{Name: name, Status: status}
		w.Utilization, _ = strconv.ParseFloat(util, 64) // Already a 0-1 fraction
		if secs, err := strconv.ParseInt(reset, 10, 64); err == nil {
			at := time.Unix(secs, 0).UTC()
			w.ResetsAt = &at
		}
		windows = append(windows, w)
	}
	if len(windows) == 0 {
		return telemetry.PlanUsage{}, false
	}
	return telemetry.PlanUsage{Provider: provider, Windows: windows, ObservedAt: now}, true
}

// turn records one /v1/messages request and its response.
type turn struct {
	rec       *Recorder
	startedAt time.Time
	body      []byte
	ready     chan struct{} // Closed once info is classified
	info      requestInfo

	// Set by response before the parser goroutine starts.
	status    int
	requestID string

	// Owned by the parser goroutine.
	span      *telemetry.InferenceSpan
	blocks    map[int]*toolBlock // Tool calls being streamed, by content index
	toolIDs   []string           // Tool calls in this response, in order
	lastDelta time.Time
}

type toolBlock struct {
	id, name string
	args     []byte
}

// begin starts recording a request. Classification runs off the request
// path, so forwarding never waits on parsing; the response parser waits for
// it instead. h and body must not be modified afterwards.
func (rec *Recorder) begin(h http.Header, body []byte) *turn {
	t := &turn{rec: rec, startedAt: time.Now(), body: body, ready: make(chan struct{}), blocks: make(map[int]*toolBlock)}
	go func() {
		t.info = classify(h, body)
		rec.requestArrived(t.info, t.startedAt)
		close(t.ready)
	}()
	return t
}

// response taps the upstream response body for the matching parser. The tap
// never blocks the stream to Claude Code.
func (t *turn) response(resp *http.Response) {
	t.status, t.requestID = resp.StatusCode, resp.Header.Get("Request-Id")
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var parse func(io.Reader)
	switch {
	case resp.StatusCode/100 != 2:
		parse = t.parseError
	case mediaType == "text/event-stream":
		parse = t.parseSSE
	case mediaType == "application/json":
		parse = t.parseJSON
	default:
		return
	}
	tap := newStreamTap()
	resp.Body = &teeBody{ReadCloser: resp.Body, tap: tap}
	go func() {
		defer tap.stop()
		<-t.ready
		raw := &cappedBuffer{limit: rawPayloadMax}
		parse(io.TeeReader(tap, raw))
		if t.span == nil {
			return
		}
		if t.span.CompletedAt == nil { // The response ended before message_stop.
			reason := "stream_ended"
			if tap.canceled() {
				reason = "client_canceled" // e.g. Esc in Claude Code
			}
			t.fail(reason)
		}
		t.rec.raw.put(t.span.ID, newRawPayload(t.body, raw))
	}()
}

// unreachable records a request that got no upstream response, because
// upstream failed or the client gave up first.
func (t *turn) unreachable(err error) {
	reason := "upstream_unreachable"
	if errors.Is(err, context.Canceled) {
		reason = "client_canceled"
	}
	go func() {
		<-t.ready
		t.errorSpan(reason)
		t.rec.raw.put(t.span.ID, newRawPayload(t.body, nil))
	}()
}

type sseUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type sseMessage struct {
	ID    string   `json:"id"`
	Model string   `json:"model"`
	Usage sseUsage `json:"usage"`
}

type sseEvent struct {
	Type         string     `json:"type"`
	Index        int        `json:"index"`
	Message      sseMessage `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage sseUsage `json:"usage"`
}

func (t *turn) parseSSE(r io.Reader) {
	sc := bufio.NewScanner(r)
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
}

func (t *turn) handle(ev sseEvent) {
	if ev.Type == "message_start" && t.span == nil {
		t.start(ev.Message)
		return
	}
	if t.span == nil || t.span.CompletedAt != nil {
		return
	}
	switch ev.Type {
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			t.toolStart(ev.Index, ev.ContentBlock.ID, ev.ContentBlock.Name)
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			t.appendText(ev.Delta.Text)
		case "input_json_delta":
			if b := t.blocks[ev.Index]; b != nil {
				b.args = append(b.args, ev.Delta.PartialJSON...)
			}
		}
	case "content_block_stop":
		t.toolArgsDone(ev.Index)
	case "message_delta":
		t.span.OutputTokens = ev.Usage.OutputTokens
		t.span.StopReason = ev.Delta.StopReason
	case "message_stop":
		t.complete()
	case "error":
		t.fail("stream_error")
	}
}

// parseJSON records a non-streaming response from its final message.
func (t *turn) parseJSON(r io.Reader) {
	defer io.Copy(io.Discard, r) // Read to the end, for the raw payload buffer.
	var m struct {
		sseMessage
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.NewDecoder(r).Decode(&m) != nil || m.ID == "" {
		return
	}
	t.start(m.sseMessage)
	for i, c := range m.Content {
		switch c.Type {
		case "text":
			t.appendText(c.Text)
		case "tool_use":
			t.toolStart(i, c.ID, c.Name)
			t.blocks[i].args = c.Input
			t.toolArgsDone(i)
		}
	}
	t.span.OutputTokens, t.span.StopReason = m.Usage.OutputTokens, m.StopReason
	t.complete()
}

func (t *turn) parseError(r io.Reader) {
	io.Copy(io.Discard, r) // Read to the end, for the raw payload buffer.
	t.errorSpan(fmt.Sprintf("http_%d", t.status))
}

func (t *turn) newSpan() *telemetry.InferenceSpan {
	return &telemetry.InferenceSpan{
		SessionID:              t.info.SessionID,
		AgentID:                t.info.AgentID,
		Surface:                t.info.Surface,
		Role:                   t.info.Role,
		Model:                  t.info.Model,
		StartedAt:              t.startedAt,
		ToolsOffered:           t.info.ToolsOffered,
		InjectedIDEBlocks:      t.info.IDEBlocks,
		SystemPromptPreview:    t.info.SystemPreview,
		LastUserMessagePreview: t.info.LastUserPreview,
	}
}

func (t *turn) start(msg sseMessage) {
	now := time.Now()
	u := msg.Usage
	sp := t.newSpan()
	sp.ID = cmp.Or(msg.ID, t.rec.nextID())
	sp.Model = cmp.Or(msg.Model, sp.Model)
	sp.Status, sp.FirstTokenAt, sp.TTFTMs = "decoding", &now, now.Sub(t.startedAt).Milliseconds()
	sp.InputTokensUncached, sp.InputTokensCacheWrite = u.InputTokens, u.CacheCreationInputTokens
	sp.InputTokensCacheRead, sp.OutputTokens = u.CacheReadInputTokens, u.OutputTokens
	if in := inputTokens(sp); in > 0 {
		sp.CacheHitRatio = float64(sp.InputTokensCacheRead) / float64(in)
	}
	t.span = sp
	t.rec.spanStarted(sp, t.info)
}

// errorSpan records a request that failed before any message started.
func (t *turn) errorSpan(reason string) {
	now := time.Now()
	sp := t.newSpan()
	sp.ID = cmp.Or(t.requestID, t.rec.nextID())
	sp.Status, sp.CompletedAt, sp.StopReason = "error", &now, reason
	sp.DurationMs = now.Sub(t.startedAt).Milliseconds()
	t.span = sp
	t.rec.spanFailed(sp, t.info, nil)
}

func (t *turn) toolStart(index int, id, name string) {
	t.blocks[index] = &toolBlock{id: id, name: name}
	t.toolIDs = append(t.toolIDs, id)
	t.rec.toolStarted(t.span, t.info, id, name)
}

func (t *turn) toolArgsDone(index int) {
	b := t.blocks[index]
	if b == nil {
		return
	}
	delete(t.blocks, index)
	target, prompt := toolTarget(b.name, b.args)
	t.rec.toolArgsDone(t.span, t.info, b.id, target, prompt)
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
	t.rec.spanCompleted(sp, t.info, t.toolIDs)
}

// fail ends a started span whose stream reported an error or broke off.
func (t *turn) fail(reason string) {
	now := time.Now()
	sp := t.span
	sp.Status, sp.CompletedAt, sp.StopReason = "error", &now, reason
	sp.DurationMs = now.Sub(sp.StartedAt).Milliseconds()
	t.rec.spanFailed(sp, t.info, t.toolIDs)
}

// requestArrived applies what a new request reports about earlier tool calls:
// their tool_result blocks finish them. The duration runs from when the tool
// started executing to this request, so it includes any wait for permission;
// lifecycle hooks will measure it exactly.
//
// A returning Task/Agent call does not finish its subagent: Claude Code runs
// subagents asynchronously, and the call returns as soon as one is launched.
func (rec *Recorder) requestArrived(info requestInfo, at time.Time) {
	if len(info.ToolResults) == 0 {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	st := rec.state(info.SessionID)
	for _, r := range info.ToolResults {
		ts, ok := st.tools[r.ToolUseID]
		if !ok {
			// The call's own events have not been parsed yet; keep the result
			// until they are.
			ts = &toolState{inv: telemetry.ToolInvocation{ToolUseID: r.ToolUseID}}
			st.tools[r.ToolUseID] = ts
		}
		if ts.finished() {
			continue
		}
		ts.inv.Status = "completed"
		if r.IsError {
			ts.inv.Status = "failed"
		}
		if start := cmp.Or(ts.runningAt, ts.argsDoneAt); !start.IsZero() {
			ts.inv.DurationMs = at.Sub(start).Milliseconds()
		}
		if ts.seen {
			rec.saveTool(ts)
		}
	}
	if a, ok := st.agents[info.AgentID]; ok && a.ActiveTool != "" {
		a.ActiveTool = ""
		rec.saveAgent(a)
	}
}

func (rec *Recorder) spanStarted(sp *telemetry.InferenceSpan, info requestInfo) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	st, agent := rec.track(sp, info)
	if sp.Role == telemetry.RoleMain {
		sp.CacheInvalidated = st.seenMain && sp.InputTokensCacheRead*2 < st.mainCacheRead &&
			sp.InputTokensUncached+sp.InputTokensCacheWrite > 10_000
		st.mainCacheRead, st.seenMain = sp.InputTokensCacheRead, true
	}
	agent.Status = "streaming"
	rec.check(rec.store.UpsertSpan(*sp))
	rec.saveAgent(agent)
	rec.broadcast(telemetry.EventSpanStarted, *sp)
}

func (rec *Recorder) spanDelta(sp *telemetry.InferenceSpan) {
	rec.check(rec.store.UpsertSpan(*sp))
	rec.broadcast(telemetry.EventSpanDelta, *sp)
}

func (rec *Recorder) toolStarted(sp *telemetry.InferenceSpan, info requestInfo, id, name string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	st, agent := rec.track(sp, info)
	ts, ok := st.tools[id]
	if !ok {
		ts = &toolState{inv: telemetry.ToolInvocation{ToolUseID: id, Status: "streaming_args"}}
		st.tools[id] = ts
	}
	ts.seen = true
	ts.inv.SpanID, ts.inv.AgentID, ts.inv.ToolName, ts.inv.StartedAt = sp.ID, sp.AgentID, name, time.Now()
	rec.saveTool(ts)
	if !ts.finished() {
		agent.ActiveTool = name
		rec.saveAgent(agent)
	}
}

func (rec *Recorder) toolArgsDone(sp *telemetry.InferenceSpan, info requestInfo, id, target, prompt string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	st, agent := rec.track(sp, info)
	ts, ok := st.tools[id]
	if !ok {
		return
	}
	ts.inv.Target, ts.prompt, ts.argsDoneAt = target, prompt, time.Now()
	rec.saveTool(ts)
	if !ts.finished() {
		agent.ActiveTool = ts.inv.ToolName
		if target != "" {
			agent.ActiveTool += ": " + target
		}
		rec.saveAgent(agent)
	}
}

func (rec *Recorder) spanCompleted(sp *telemetry.InferenceSpan, info requestInfo, toolIDs []string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	st, agent := rec.track(sp, info)
	// Claude Code runs the response's tool calls once the message ends.
	now := time.Now()
	pending := false
	for _, id := range toolIDs {
		ts, ok := st.tools[id]
		if !ok {
			continue
		}
		if !ts.finished() {
			ts.inv.Status, ts.runningAt, pending = "running", now, true
			rec.saveTool(ts)
		}
		sp.ToolsInvoked = append(sp.ToolsInvoked, ts.inv)
	}
	agent.TotalTokens += inputTokens(sp) + sp.OutputTokens
	agent.Status = "idle"
	if pending {
		agent.Status = "running_tool"
	} else {
		agent.ActiveTool = ""
	}
	rec.check(rec.store.UpsertSpan(*sp))
	rec.broadcast(telemetry.EventSpanCompleted, *sp)
	rec.saveAgent(agent)

	sess := st.session
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
	sess.UpdatedAt = now
	rec.check(rec.store.UpsertSession(*sess))
	rec.broadcast(telemetry.EventSessionUpdated, *sess)
}

func (rec *Recorder) spanFailed(sp *telemetry.InferenceSpan, info requestInfo, toolIDs []string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	st, agent := rec.track(sp, info)
	agent.Status = "error"
	rec.check(rec.store.UpsertSpan(*sp))
	rec.broadcast(telemetry.EventSpanCompleted, *sp)
	for _, id := range toolIDs {
		if ts, ok := st.tools[id]; ok && ts.inv.Status == "streaming_args" {
			ts.inv.Status = "failed"
			rec.saveTool(ts)
		}
	}
	rec.saveAgent(agent)
}

// track returns the span's session state and agent node, creating and saving
// the session and agent on first sight. A new subagent is labelled with its
// task and linked to the Task/Agent call that spawned it. Callers hold rec.mu.
func (rec *Recorder) track(sp *telemetry.InferenceSpan, info requestInfo) (*sessionState, *telemetry.AgentNode) {
	st := rec.state(sp.SessionID)
	if st.session == nil && sp.SessionID != "" {
		st.session = &telemetry.Session{
			ID:               sp.SessionID,
			Provider:         provider,
			PrimarySurface:   info.Surface,
			StartedAt:        sp.StartedAt,
			UpdatedAt:        time.Now(),
			MaxContextWindow: telemetry.DefaultMaxContextWindow,
		}
		rec.check(rec.store.UpsertSession(*st.session))
		rec.broadcast(telemetry.EventSessionUpdated, *st.session)
	}
	a, ok := st.agents[sp.AgentID]
	if !ok {
		a = &telemetry.AgentNode{
			ID:            sp.AgentID,
			SessionID:     sp.SessionID,
			ParentAgentID: info.ParentAgentID,
			Role:          info.AgentRole,
			SubagentType:  info.SubagentType,
			StartedAt:     sp.StartedAt,
		}
		if info.AgentRole == telemetry.RoleSubagent {
			a.Label = truncateRunes(strings.Join(strings.Fields(info.FirstUserText), " "), labelRunes)
			a.ParentToolID = matchTask(st, info)
		}
		st.agents[a.ID] = a
	}
	a.Surface = sp.Surface
	// A helper call made on an agent's behalf does not relabel its model.
	if a.Model == "" || sp.Role == a.Role {
		a.Model = sp.Model
	}
	return st, a
}

// matchTask finds the parent's not-yet-linked Task/Agent call whose prompt
// the subagent's first message contains, and marks it linked. The call may
// already have returned, since subagents run asynchronously. No match, or
// more than one, links nothing: PostToolUse's agentId will correct it once
// hooks exist.
func matchTask(st *sessionState, info requestInfo) string {
	if info.FirstUserText == "" {
		return ""
	}
	var match *toolState
	for _, ts := range st.tools {
		if ts.linked || ts.prompt == "" || ts.inv.AgentID != info.ParentAgentID ||
			!strings.Contains(info.FirstUserText, ts.prompt) {
			continue
		}
		if match != nil {
			return ""
		}
		match = ts
	}
	if match == nil {
		return ""
	}
	match.linked = true
	return match.inv.ToolUseID
}

// state returns the working state for a session, marking it most recently
// written; creating one may drop the least recently written. Callers hold
// rec.mu.
func (rec *Recorder) state(sessionID string) *sessionState {
	rec.writes++
	if st, ok := rec.live[sessionID]; ok {
		st.writtenAt = rec.writes
		return st
	}
	if len(rec.live) >= telemetry.MaxLiveSessions {
		var oldestID string
		var oldest *sessionState
		for id, st := range rec.live {
			if oldest == nil || st.writtenAt < oldest.writtenAt {
				oldestID, oldest = id, st
			}
		}
		delete(rec.live, oldestID)
	}
	st := &sessionState{
		agents:    make(map[string]*telemetry.AgentNode),
		tools:     make(map[string]*toolState),
		writtenAt: rec.writes,
	}
	rec.live[sessionID] = st
	return st
}

func (rec *Recorder) saveAgent(a *telemetry.AgentNode) {
	rec.check(rec.store.UpsertAgent(*a))
	rec.broadcast(telemetry.EventAgentUpserted, *a)
}

func (rec *Recorder) saveTool(ts *toolState) {
	rec.check(rec.store.UpsertTool(ts.inv))
	rec.broadcast(telemetry.EventToolUpdated, ts.inv)
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

func (rec *Recorder) nextID() string {
	return fmt.Sprintf("atap-%d", rec.seq.Add(1))
}

// toolTarget extracts what a tool call acts on from its arguments: a path,
// command or pattern, or for Task/Agent the description, plus the full prompt.
func toolTarget(name string, args []byte) (target, prompt string) {
	var in map[string]any
	if json.Unmarshal(args, &in) != nil {
		return "", ""
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	if name == "Task" || name == "Agent" {
		prompt = str("prompt")
		target = cmp.Or(str("description"), prompt)
	} else {
		for _, k := range []string{"file_path", "command", "pattern", "path", "url", "query"} {
			if target = str(k); target != "" {
				break
			}
		}
	}
	return truncateRunes(target, targetRunes), prompt
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
