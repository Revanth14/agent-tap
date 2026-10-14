package claudecode

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Revanth14/agent-tap/internal/telemetry"
)

// sendHook posts a hook payload to the recorder's handler and returns the
// response.
func sendHook(t *testing.T, rec *Recorder, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/claude", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()
	rec.HookHandler().ServeHTTP(rr, req)
	return rr
}

func hook(t *testing.T, rec *Recorder, body string) {
	t.Helper()
	if rr := sendHook(t, rec, "application/json", body); rr.Code != http.StatusOK || rr.Body.String() != "{}" {
		t.Fatalf("hook %s: %d %q, want 200 {}", body, rr.Code, rr.Body)
	}
}

func TestHookHandlerAcceptsOnlyJSON(t *testing.T) {
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(store, nil)
	// A cross-site page can send text/plain without a CORS preflight.
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		if rr := sendHook(t, rec, ct, `{"session_id":"s","hook_event_name":"SessionStart"}`); rr.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q: status %d, want 415", ct, rr.Code)
		}
	}
	if len(store.Snapshot().Sessions) != 0 {
		t.Error("a rejected hook was recorded")
	}
	hook(t, rec, `{"session_id":"s","hook_event_name":"SessionStart","cwd":"/repo"}`)
	if rr := sendHook(t, rec, "application/json; charset=utf-8", `{"session_id":"s","hook_event_name":"Stop"}`); rr.Code != http.StatusOK {
		t.Errorf("JSON with charset: status %d, want 200", rr.Code)
	}
	if rr := sendHook(t, rec, "application/json", `{not json`); rr.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status %d, want 400", rr.Code)
	}
}

func TestHookBeforeFirstRequestCreatesSessionWithWorkspace(t *testing.T) {
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(store, nil)
	hook(t, rec, `{"session_id":"s","hook_event_name":"SessionStart","cwd":"/repo"}`)
	snap := store.Snapshot()
	if len(snap.Sessions) != 1 || snap.Sessions[0].WorkspacePath != "/repo" || snap.Sessions[0].PrimarySurface != telemetry.SurfaceUnknown {
		t.Fatalf("sessions = %+v, want s at /repo with an unknown surface", snap.Sessions)
	}

	info := requestInfo{SessionID: "s", AgentID: "s/main", AgentRole: telemetry.RoleMain, Role: telemetry.RoleMain, Surface: telemetry.SurfaceVSCodeExt}
	rec.spanStarted(&telemetry.InferenceSpan{ID: "m1", SessionID: "s", AgentID: "s/main", Role: telemetry.RoleMain, Surface: telemetry.SurfaceVSCodeExt}, info)
	if s := store.Snapshot().Sessions[0]; s.PrimarySurface != telemetry.SurfaceVSCodeExt || s.WorkspacePath != "/repo" {
		t.Errorf("session = %+v, want the first request to fill in the surface and keep the workspace", s)
	}
}

func TestHooksGiveExactToolTiming(t *testing.T) {
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(store, nil)
	info := requestInfo{SessionID: "s", AgentID: "s/main", AgentRole: telemetry.RoleMain, Role: telemetry.RoleMain}
	sp := &telemetry.InferenceSpan{ID: "m1", SessionID: "s", AgentID: "s/main", Role: telemetry.RoleMain}
	rec.spanStarted(sp, info)
	rec.toolStarted(sp, info, "t1", "Bash")
	rec.toolArgsDone(sp, info, "t1", "go test ./...", "")

	hook(t, rec, `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"t1"}`)
	time.Sleep(30 * time.Millisecond)
	rec.spanCompleted(sp, info, []string{"t1"}) // A late message_stop must keep PreToolUse's start time.
	hook(t, rec, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"t1","tool_response":{"stdout":"ok"}}`)
	rec.requestArrived(requestInfo{SessionID: "s", AgentID: "s/main", ToolResults: []toolResult{{ToolUseID: "t1"}}}, time.Now().Add(time.Hour))

	tl := store.Snapshot().Tools[0]
	if tl.Status != "completed" || tl.DurationMs < 30 || tl.DurationMs > 1000 {
		t.Errorf("tool = %+v, want completed with the hooks' ~30ms duration", tl)
	}

	// Claude Code's own duration_ms wins over the PreToolUse gap, which can
	// include a permission prompt.
	rec.toolStarted(sp, info, "t3", "Bash")
	hook(t, rec, `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"t3"}`)
	time.Sleep(30 * time.Millisecond)
	hook(t, rec, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"t3","duration_ms":7,"tool_response":{}}`)
	for _, tl := range store.Snapshot().Tools {
		if tl.ToolUseID == "t3" && tl.DurationMs != 7 {
			t.Errorf("tool t3 DurationMs = %d, want Claude Code's duration_ms of 7", tl.DurationMs)
		}
	}

	// A tool whose hooks arrive before the parser reaches its tool_use block.
	hook(t, rec, `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Read","tool_use_id":"t2"}`)
	hook(t, rec, `{"session_id":"s","hook_event_name":"PostToolUseFailure","tool_name":"Read","tool_use_id":"t2"}`)
	rec.toolStarted(sp, info, "t2", "Read")
	rec.toolArgsDone(sp, info, "t2", "/a.go", "")
	rec.spanCompleted(sp, info, []string{"t2"})
	for _, tl := range store.Snapshot().Tools {
		if tl.ToolUseID == "t2" && (tl.Status != "failed" || tl.ToolName != "Read") {
			t.Errorf("early-hooked tool = %+v, want failed, named once the parser caught up", tl)
		}
	}
}

func TestHooksLinkAndCompleteSubagents(t *testing.T) {
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(store, nil)
	mainInfo := requestInfo{SessionID: "s", AgentID: "s/main", AgentRole: telemetry.RoleMain, Role: telemetry.RoleMain}
	sp := &telemetry.InferenceSpan{ID: "m1", SessionID: "s", AgentID: "s/main", Role: telemetry.RoleMain}
	rec.spanStarted(sp, mainInfo)
	for _, id := range []string{"toolu_a", "toolu_b"} {
		rec.toolStarted(sp, mainInfo, id, "Agent")
		rec.toolArgsDone(sp, mainInfo, id, "task", "a prompt the subagents' messages do not contain")
	}
	rec.spanCompleted(sp, mainInfo, []string{"toolu_a", "toolu_b"})

	subInfo := func(id string) requestInfo {
		return requestInfo{SessionID: "s", AgentID: id, ParentAgentID: "s/main", AgentRole: telemetry.RoleSubagent, Role: telemetry.RoleSubagent, FirstUserText: "unrelated text"}
	}
	subSpan := func(id string) *telemetry.InferenceSpan {
		return &telemetry.InferenceSpan{ID: "span-" + id, SessionID: "s", AgentID: id, Role: telemetry.RoleSubagent}
	}

	// a1 sent its first request before PostToolUse: the prompt match fails,
	// and PostToolUse's agentId corrects the link.
	rec.spanStarted(subSpan("a1"), subInfo("a1"))
	hook(t, rec, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Agent","tool_use_id":"toolu_a","tool_response":{"status":"async_launched","agentId":"a1"}}`)
	// a2's PostToolUse came first: the link waits for a2's first request.
	hook(t, rec, `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Agent","tool_use_id":"toolu_b","tool_response":{"agentId":"a2"}}`)
	rec.spanStarted(subSpan("a2"), subInfo("a2"))

	hook(t, rec, `{"session_id":"s","hook_event_name":"SubagentStop","agent_id":"a1"}`)

	agents := map[string]telemetry.AgentNode{}
	for _, a := range store.Snapshot().Agents {
		agents[a.ID] = a
	}
	if a := agents["a1"]; a.ParentToolID != "toolu_a" || a.Status != "completed" || a.CompletedAt == nil {
		t.Errorf("a1 = %+v, want linked by agentId and completed by SubagentStop", a)
	}
	if a := agents["a2"]; a.ParentToolID != "toolu_b" || a.Status == "completed" {
		t.Errorf("a2 = %+v, want linked from the held agentId and still running", a)
	}
}

func TestWireLogRecordsHookKeysNeverValues(t *testing.T) {
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(store, nil)
	var wire syncBuffer
	if _, err := NewProxy("https://api.anthropic.com", rec, &wire); err != nil {
		t.Fatal(err)
	}
	hook(t, rec, `{"session_id":"s","hook_event_name":"PostToolUse","cwd":"/CWD-SECRET","tool_name":"Bash","tool_use_id":"t1",`+
		`"tool_input":{"command":"COMMAND-SECRET"},"tool_response":{"stdout":"STDOUT-SECRET","agentId":"a1"}}`)

	log := wire.String()
	for _, secret := range []string{"CWD-SECRET", "COMMAND-SECRET", "STDOUT-SECRET"} {
		if strings.Contains(log, secret) {
			t.Errorf("wire log contains %s:\n%s", secret, log)
		}
	}
	for _, want := range []string{`"event":"PostToolUse"`, `"tool_input"`, `"tool_response_keys":["agentId","stdout"]`} {
		if !strings.Contains(log, want) {
			t.Errorf("wire log lacks %s:\n%s", want, log)
		}
	}
}
