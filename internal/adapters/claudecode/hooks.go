package claudecode

import (
	"encoding/json"
	"io"
	"maps"
	"mime"
	"net/http"
	"slices"
	"time"

	"github.com/Revanth14/agent-tap/internal/telemetry"
)

const hookBodyMax = 16 << 20 // PostToolUse carries whole tool responses

// hookEvent is the part of a Claude Code hook payload the recorder uses.
type hookEvent struct {
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolUseID     string          `json:"tool_use_id"`
	ToolResponse  json.RawMessage `json:"tool_response"`
	DurationMs    *int64          `json:"duration_ms"` // PostToolUse: Claude Code's own measure of the tool's run
	AgentID       string          `json:"agent_id"`
}

// Routes returns the adapter's HTTP routes, served by the daemon next to its
// own.
func (rec *Recorder) Routes() map[string]http.Handler {
	return map[string]http.Handler{
		"GET /api/spans/{id}/raw": rec.RawHandler(),
		"POST /api/hooks/claude":  rec.HookHandler(),
	}
}

// HookHandler serves POST /api/hooks/claude, the target of Claude Code's
// native HTTP hooks. Only JSON is accepted: a cross-site page cannot send it
// without a CORS preflight it would fail. Every accepted hook gets 200 {}, so
// Claude Code never waits on or reacts to the daemon.
func (rec *Recorder) HookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" {
			http.Error(w, "atap: hooks must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hookBodyMax))
		if err != nil {
			http.Error(w, "atap: read hook: "+err.Error(), http.StatusBadRequest)
			return
		}
		var ev hookEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "atap: decode hook: "+err.Error(), http.StatusBadRequest)
			return
		}
		if rec.wire != nil {
			rec.wire.hook(ev.HookEventName, body)
		}
		rec.applyHook(ev, time.Now())
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{}")
	})
}

// applyHook records a hook event. Hooks are the exact source for tool timing
// and subagent completion; they can arrive before or after the parser's view
// of the same tool, so nothing here moves state backwards.
func (rec *Recorder) applyHook(ev hookEvent, at time.Time) {
	if ev.SessionID == "" {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	st := rec.state(ev.SessionID)
	rec.setWorkspace(st, ev.SessionID, ev.Cwd, at)

	switch ev.HookEventName {
	case "PreToolUse":
		ts := rec.hookTool(st, ev.ToolUseID)
		if ts == nil || ts.finished() {
			return
		}
		ts.inv.Status, ts.runningAt, ts.hookStarted = "running", at, true
		rec.saveToolIfSeen(ts)

	case "PostToolUse", "PostToolUseFailure":
		ts := rec.hookTool(st, ev.ToolUseID)
		if ts == nil {
			return
		}
		ts.inv.Status = "completed"
		if ev.HookEventName == "PostToolUseFailure" {
			ts.inv.Status = "failed"
		}
		// Claude Code's duration_ms excludes any wait for a permission prompt,
		// which PreToolUse precedes; the PreToolUse gap is the fallback.
		switch {
		case ev.DurationMs != nil:
			ts.inv.DurationMs = *ev.DurationMs
		case ts.hookStarted:
			ts.inv.DurationMs = at.Sub(ts.runningAt).Milliseconds()
		}
		rec.saveToolIfSeen(ts)
		if ev.ToolName == "Agent" || ev.ToolName == "Task" {
			if agentID := responseAgentID(ev.ToolResponse); agentID != "" {
				rec.linkSubagent(st, agentID, ev.ToolUseID)
			}
		}

	case "SubagentStop":
		if a, ok := st.agents[ev.AgentID]; ok && a.Status != "completed" {
			a.Status, a.CompletedAt, a.ActiveTool = "completed", &at, ""
			rec.saveAgent(a)
		}
	}
}

// hookTool returns the tool a hook refers to, holding a placeholder when the
// parser has not reached its tool_use block yet. Callers hold rec.mu.
func (rec *Recorder) hookTool(st *sessionState, toolUseID string) *toolState {
	if toolUseID == "" {
		return nil
	}
	ts, ok := st.tools[toolUseID]
	if !ok {
		ts = &toolState{inv: telemetry.ToolInvocation{ToolUseID: toolUseID}}
		st.tools[toolUseID] = ts
	}
	return ts
}

// linkSubagent sets a subagent's ParentToolID from PostToolUse's agentId,
// which is authoritative over the prompt match. A subagent that has not sent
// a request yet gets the link when it does. Callers hold rec.mu.
func (rec *Recorder) linkSubagent(st *sessionState, agentID, toolUseID string) {
	if ts, ok := st.tools[toolUseID]; ok {
		ts.linked = true
	}
	a, ok := st.agents[agentID]
	if !ok {
		st.pendingLinks[agentID] = toolUseID
		return
	}
	if a.ParentToolID != toolUseID {
		a.ParentToolID = toolUseID
		rec.saveAgent(a)
	}
}

// setWorkspace records a hook's cwd as the session's workspace, creating the
// session if a hook arrives before its first request. Callers hold rec.mu.
func (rec *Recorder) setWorkspace(st *sessionState, sessionID, cwd string, at time.Time) {
	if st.session == nil {
		st.session = &telemetry.Session{
			ID:               sessionID,
			Provider:         provider,
			PrimarySurface:   telemetry.SurfaceUnknown,
			StartedAt:        at,
			UpdatedAt:        at,
			MaxContextWindow: telemetry.DefaultMaxContextWindow,
		}
	} else if cwd == "" || cwd == st.session.WorkspacePath {
		return
	}
	if cwd != "" {
		st.session.WorkspacePath = cwd
	}
	rec.check(rec.store.UpsertSession(*st.session))
	rec.broadcast(telemetry.EventSessionUpdated, *st.session)
}

func (rec *Recorder) saveToolIfSeen(ts *toolState) {
	if ts.seen {
		rec.saveTool(ts)
	}
}

// responseAgentID reads the subagent ID from an Agent/Task tool response.
func responseAgentID(raw json.RawMessage) string {
	var resp map[string]any
	if json.Unmarshal(raw, &resp) != nil {
		return ""
	}
	for _, k := range []string{"agentId", "agent_id"} {
		if id, ok := resp[k].(string); ok && id != "" {
			return id
		}
	}
	return ""
}

// hookKeys returns a hook payload's top-level keys and its tool_response's
// keys, for the wire log; values are never logged.
func hookKeys(body []byte) (keys, toolResponseKeys []string) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil, nil
	}
	keys = slices.Sorted(maps.Keys(payload))
	var resp map[string]json.RawMessage
	if json.Unmarshal(payload["tool_response"], &resp) == nil {
		toolResponseKeys = slices.Sorted(maps.Keys(resp))
	}
	return keys, toolResponseKeys
}
