// Package demo drives simulated Claude Code traffic through the real Claude
// Code adapter, so the UI can be inspected without live traffic or plan
// usage. A fake upstream on a loopback port answers a dedicated proxy that
// shares the daemon's recorder; real traffic keeps flowing to Anthropic
// through the daemon's own proxy alongside it.
//
// The demo sends no usage-limit headers: plan usage is account-level, and
// fake values would overwrite real ones.
package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Revanth14/agent-tap/internal/adapters/claudecode"
)

const (
	replyHeader = "X-Atap-Demo-Reply" // Tells the fake upstream which scripted reply to stream
	workspace   = "/Users/demo/acme-api"
	vscodeUA    = "claude-cli/2.1.299 (external, claude-vscode, agent-sdk/0.3.299)"
	cliUA       = "claude-cli/2.1.299 (external, cli)"
	mainModel   = "claude-opus-5-5"
	fastModel   = "claude-haiku-5-5"
)

var (
	mainTools = []string{"Agent", "Bash", "Edit", "Glob", "Grep", "Read", "TodoWrite", "WebFetch", "Write"}
	subTools  = []string{"Bash", "Glob", "Grep", "Read"}
)

// Options paces the simulation.
type Options struct {
	Speed float64       // 1 is real time; larger runs faster
	Every time.Duration // Pause between scenario runs (Run only)
}

// Run plays the scenario repeatedly, with new session IDs each time, until ctx
// ends.
func Run(ctx context.Context, rec *claudecode.Recorder, opts Options) error {
	s, err := newSim(rec, opts)
	if err != nil {
		return err
	}
	defer s.close()
	for n := 1; ; n++ {
		if err := s.scenario(ctx, n); err != nil {
			return ignoreCanceled(err)
		}
		if !s.sleep(ctx, opts.Every) {
			return nil
		}
	}
}

// RunOnce plays the scenario once.
func RunOnce(ctx context.Context, rec *claudecode.Recorder, opts Options) error {
	s, err := newSim(rec, opts)
	if err != nil {
		return err
	}
	defer s.close()
	return s.scenario(ctx, 1)
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

type sim struct {
	rec      *claudecode.Recorder
	proxy    *claudecode.Proxy
	upstream *http.Server
	speed    float64
	seq      atomic.Uint64

	mu      sync.Mutex
	replies map[string]reply
}

func newSim(rec *claudecode.Recorder, opts Options) (*sim, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("demo: listen: %w", err)
	}
	s := &sim{rec: rec, speed: opts.Speed, replies: make(map[string]reply)}
	if s.speed <= 0 {
		s.speed = 1
	}
	s.upstream = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 10 * time.Second}
	go s.upstream.Serve(ln)
	if s.proxy, err = claudecode.NewProxy("http://"+ln.Addr().String(), rec, nil); err != nil {
		s.upstream.Close()
		return nil, err
	}
	return s, nil
}

func (s *sim) close() { s.upstream.Close() }

// sleep waits d scaled by the speed, reporting false if ctx ended first.
func (s *sim) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(time.Duration(float64(d) / s.speed))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (s *sim) id(prefix string) string { return fmt.Sprintf("%s_demo_%04d", prefix, s.seq.Add(1)) }

// toolCall is one tool_use block in a scripted reply, with how the tool
// "runs" afterwards.
type toolCall struct {
	id, name string
	input    map[string]any
	runFor   time.Duration
	response map[string]any // PostToolUse tool_response
}

// reply is a scripted response streamed by the fake upstream.
type reply struct {
	id, model                       string
	ttft                            time.Duration
	text                            string
	tools                           []toolCall
	uncached, cacheWrite, cacheRead int
	output                          int
}

// agentCall identifies who sends a request, as Claude Code's headers do.
type agentCall struct {
	session, userAgent, class string
	agentID, agentType        string
	system                    string
	model                     string
	tools                     []string
}

// call sends one /v1/messages request through the proxy and waits for the
// whole response. Like real Claude Code, the request carries the
// conversation so far.
func (s *sim) call(ctx context.Context, a agentCall, messages []any, rep reply) error {
	rep.id = s.id("msg")
	if rep.model == "" {
		rep.model = a.model
	}
	key := s.id("reply")
	s.mu.Lock()
	s.replies[key] = rep
	s.mu.Unlock()

	tools := make([]map[string]string, len(a.tools))
	for i, name := range a.tools {
		tools[i] = map[string]string{"name": name}
	}
	body, err := json.Marshal(map[string]any{
		"model":    a.model,
		"stream":   true,
		"system":   []map[string]string{{"type": "text", "text": a.system}},
		"messages": messages,
		"tools":    tools,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1/v1/messages?beta=true", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", a.userAgent)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-Claude-Code-Session-Id", a.session)
	req.Header.Set("X-Claude-Code-Request-Class", a.class)
	if a.agentID != "" {
		req.Header.Set("X-Claude-Code-Agent-Id", a.agentID)
		req.Header.Set("X-Claude-Code-Agent-Type", a.agentType)
	}
	req.Header.Set(replyHeader, key)
	s.proxy.ServeHTTP(&discard{}, req)
	return ctx.Err()
}

// runTools plays each tool call's hooks: PreToolUse, the run, PostToolUse
// with Claude Code's duration_ms.
func (s *sim) runTools(ctx context.Context, a agentCall, rep reply) error {
	for _, tc := range rep.tools {
		base := map[string]any{
			"session_id": a.session, "cwd": workspace,
			"tool_name": tc.name, "tool_use_id": tc.id, "tool_input": tc.input,
		}
		if a.agentID != "" {
			base["agent_id"], base["agent_type"] = a.agentID, a.agentType
		}
		s.hook(with(base, "hook_event_name", "PreToolUse"))
		if !s.sleep(ctx, tc.runFor) {
			return ctx.Err()
		}
		post := with(base, "hook_event_name", "PostToolUse")
		post["duration_ms"] = tc.runFor.Milliseconds()
		post["tool_response"] = tc.response
		s.hook(post)
	}
	return nil
}

func (s *sim) hook(payload map[string]any) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/api/hooks/claude", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.rec.HookHandler().ServeHTTP(&discard{}, req)
}

func with(m map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for key, val := range m {
		out[key] = val
	}
	out[k] = v
	return out
}

// Conversation helpers, in the Messages API's shape.

func userText(texts ...string) map[string]any {
	blocks := make([]map[string]string, len(texts))
	for i, t := range texts {
		blocks[i] = map[string]string{"type": "text", "text": t}
	}
	return map[string]any{"role": "user", "content": blocks}
}

func assistant(rep reply) map[string]any {
	var blocks []map[string]any
	if rep.text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": rep.text})
	}
	for _, tc := range rep.tools {
		blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.id, "name": tc.name, "input": tc.input})
	}
	return map[string]any{"role": "assistant", "content": blocks}
}

func results(rep reply, content string) map[string]any {
	blocks := make([]map[string]any, len(rep.tools))
	for i, tc := range rep.tools {
		blocks[i] = map[string]any{"type": "tool_result", "tool_use_id": tc.id, "content": content}
	}
	return map[string]any{"role": "user", "content": blocks}
}

// serve streams the scripted reply named by the request, at a realistic pace.
func (s *sim) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	rep, ok := s.replies[r.Header.Get(replyHeader)]
	delete(s.replies, r.Header.Get(replyHeader))
	s.mu.Unlock()
	if !ok {
		http.Error(w, "demo: unknown reply", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(ev map[string]any) {
		data, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	ctx := r.Context()
	if !s.sleep(ctx, rep.ttft) {
		return
	}
	send(map[string]any{"type": "message_start", "message": map[string]any{
		"id": rep.id, "type": "message", "role": "assistant", "model": rep.model, "content": []any{},
		"usage": map[string]int{
			"input_tokens": rep.uncached, "cache_creation_input_tokens": rep.cacheWrite,
			"cache_read_input_tokens": rep.cacheRead, "output_tokens": 1,
		},
	}})
	index := 0
	if rep.text != "" {
		send(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]string{"type": "text", "text": ""}})
		for _, chunk := range chunks(rep.text, 14) {
			if !s.sleep(ctx, 45*time.Millisecond) {
				return
			}
			send(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]string{"type": "text_delta", "text": chunk}})
		}
		send(map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	for _, tc := range rep.tools {
		send(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": tc.id, "name": tc.name, "input": map[string]any{}}})
		args, _ := json.Marshal(tc.input)
		for _, chunk := range chunks(string(args), 24) {
			if !s.sleep(ctx, 30*time.Millisecond) {
				return
			}
			send(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]string{"type": "input_json_delta", "partial_json": chunk}})
		}
		send(map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	stop := "end_turn"
	if len(rep.tools) > 0 {
		stop = "tool_use"
	}
	send(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]int{"output_tokens": rep.output}})
	send(map[string]any{"type": "message_stop"})
}

func chunks(s string, n int) []string {
	var out []string
	r := []rune(s)
	for len(r) > n {
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	return append(out, string(r))
}

// discard is a ResponseWriter for in-process calls whose responses only
// matter to the recorder.
type discard struct{ h http.Header }

func (d *discard) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *discard) Write(p []byte) (int, error) { return len(p), nil }
func (d *discard) WriteHeader(int)             {}
func (d *discard) Flush()                      {}
