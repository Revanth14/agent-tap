package telemetry

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreEmptySnapshotEncodesArrays(t *testing.T) {
	s, err := OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"sessions":[],"agents":[],"spans":[],"tools":[],"plan_usage":null}`; string(data) != want {
		t.Errorf("empty snapshot = %s, want %s", data, want)
	}
}

func TestStoreSnapshot(t *testing.T) {
	s, err := OpenStore("", 2)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	mustNoErr(t, s.UpsertSession(Session{ID: "s-late", StartedAt: t0.Add(time.Minute)}))
	mustNoErr(t, s.UpsertSession(Session{ID: "s-early", StartedAt: t0}))
	mustNoErr(t, s.UpsertAgent(AgentNode{ID: "a-sub", SessionID: "s-early", Role: RoleSubagent, StartedAt: t0.Add(time.Second)}))
	mustNoErr(t, s.UpsertAgent(AgentNode{ID: "a-main", SessionID: "s-early", Role: RoleMain, StartedAt: t0}))

	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "sp1", Status: "prefill"}))
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "sp2", Status: "prefill"}))
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "sp1", Status: "completed"})) // update in place, keeps position
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "sp3", Status: "decoding"}))  // evicts sp1
	mustNoErr(t, s.UpsertTool(ToolInvocation{ToolUseID: "tu1", SpanID: "sp2", ToolName: "Read"}))

	snap := s.Snapshot()
	if got := ids(snap.Sessions, func(v Session) string { return v.ID }); got != "s-early,s-late" {
		t.Errorf("sessions = %s, want s-early,s-late", got)
	}
	if got := ids(snap.Agents, func(v AgentNode) string { return v.ID }); got != "a-main,a-sub" {
		t.Errorf("agents = %s, want a-main,a-sub", got)
	}
	if got := ids(snap.Spans, func(v InferenceSpan) string { return v.ID + ":" + v.Status }); got != "sp2:prefill,sp3:decoding" {
		t.Errorf("spans = %s, want sp2:prefill,sp3:decoding", got)
	}
	if got := ids(snap.Tools, func(v ToolInvocation) string { return v.ToolUseID }); got != "tu1" {
		t.Errorf("tools = %s, want tu1", got)
	}

	if err := s.UpsertSpan(InferenceSpan{}); err == nil {
		t.Error("UpsertSpan with empty ID: want error")
	}
}

func TestStoreCopiesSpanSlices(t *testing.T) {
	s, err := OpenStore("", 0)
	mustNoErr(t, err)
	in := []string{"Read"}
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "x", ToolsOffered: in}))
	in[0] = "MUTATED"
	if got := s.Snapshot().Spans[0].ToolsOffered[0]; got != "Read" {
		t.Errorf("stored ToolsOffered[0] = %q, want Read (caller mutation leaked)", got)
	}
}

func TestStoreRingsArePerSession(t *testing.T) {
	s, err := OpenStore("", 2)
	mustNoErr(t, err)
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "a1", SessionID: "A"}))
	mustNoErr(t, s.UpsertTool(ToolInvocation{ToolUseID: "ta", SpanID: "a1"}))
	for _, id := range []string{"b1", "b2", "b3"} {
		mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: id, SessionID: "B"}))
		mustNoErr(t, s.UpsertTool(ToolInvocation{ToolUseID: "t" + id, SpanID: id}))
	}

	snap := s.Snapshot()
	if got := ids(snap.Spans, func(v InferenceSpan) string { return v.ID }); got != "a1,b2,b3" {
		t.Errorf("spans = %s, want a1,b2,b3 (B must not evict A)", got)
	}
	if got := ids(snap.Tools, func(v ToolInvocation) string { return v.ToolUseID }); got != "ta,tb2,tb3" {
		t.Errorf("tools = %s, want ta,tb2,tb3", got)
	}
}

func TestStoreEvictsLeastRecentlyWrittenSession(t *testing.T) {
	s, err := OpenStore("", 0)
	mustNoErr(t, err)
	sessionID := func(i int) string { return fmt.Sprintf("s%02d", i) }
	for i := range MaxLiveSessions {
		mustNoErr(t, s.UpsertSession(Session{ID: sessionID(i)}))
		mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "span-" + sessionID(i), SessionID: sessionID(i)}))
	}
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "span-s00-2", SessionID: "s00"})) // s00 is now the most recent
	mustNoErr(t, s.UpsertSession(Session{ID: "new"}))                             // evicts s01

	snap := s.Snapshot()
	sessions := "," + ids(snap.Sessions, func(v Session) string { return v.ID }) + ","
	spans := "," + ids(snap.Spans, func(v InferenceSpan) string { return v.ID }) + ","
	if len(snap.Sessions) != MaxLiveSessions {
		t.Errorf("live sessions = %d, want %d", len(snap.Sessions), MaxLiveSessions)
	}
	if strings.Contains(sessions, ",s01,") || strings.Contains(spans, ",span-s01,") {
		t.Errorf("s01 still live: sessions %s spans %s", sessions, spans)
	}
	if !strings.Contains(sessions, ",s00,") || !strings.Contains(sessions, ",new,") {
		t.Errorf("sessions = %s, want s00 and new kept", sessions)
	}
}

func TestStoreToolNeedsKnownSpanOrAgent(t *testing.T) {
	s, err := OpenStore("", 0)
	mustNoErr(t, err)
	if err := s.UpsertTool(ToolInvocation{ToolUseID: "tu1", SpanID: "missing"}); !errors.Is(err, errToolNoSession) {
		t.Errorf("UpsertTool with unknown span: err = %v, want errToolNoSession", err)
	}

	mustNoErr(t, s.UpsertAgent(AgentNode{ID: "a1", SessionID: "S"}))
	mustNoErr(t, s.UpsertTool(ToolInvocation{ToolUseID: "tu1", AgentID: "a1"}))
	if got := len(s.Snapshot().Tools); got != 1 {
		t.Errorf("tools = %d, want 1 (joined via agent)", got)
	}
}

func TestStorePlanUsage(t *testing.T) {
	s, err := OpenStore("", 0)
	mustNoErr(t, err)
	if s.Snapshot().PlanUsage != nil {
		t.Fatal("PlanUsage before any update: want nil")
	}

	s.SetPlanUsage(PlanUsage{Provider: "claude_code", Windows: []UsageWindow{{Name: "5h", Utilization: 0.42, Status: "allowed"}}})
	reset := time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)
	windows := []UsageWindow{
		{Name: "5h", Utilization: 0.46, ResetsAt: &reset, Status: "allowed"},
		{Name: "7d", Utilization: 0.13, Status: "allowed"},
	}
	s.SetPlanUsage(PlanUsage{Provider: "claude_code", Windows: windows})
	// The store must hold its own copy of the windows and their reset times.
	windows[0].Utilization = 0.99
	reset = time.Time{}

	got := s.Snapshot().PlanUsage
	if got == nil || len(got.Windows) != 2 {
		t.Fatalf("PlanUsage = %+v, want the latest two windows", got)
	}
	if w := got.Windows[0]; w.Utilization != 0.46 || w.ResetsAt == nil || !w.ResetsAt.Equal(time.Date(2026, 10, 14, 19, 0, 0, 0, time.UTC)) {
		t.Errorf("5h window = %+v, want the latest value, unaffected by caller mutation", w)
	}
}

func TestStorePersistsToSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.db")
	s, err := OpenStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	mustNoErr(t, s.UpsertSession(Session{ID: "s1", Provider: "claude_code"}))
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "sp1", SessionID: "s1", AgentID: "a1", Status: "prefill"}))
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "sp1", SessionID: "s1", AgentID: "a1", Status: "completed", OutputTokens: 42}))
	mustNoErr(t, s.Close())

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var n int
	mustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM spans`).Scan(&n))
	if n != 1 {
		t.Fatalf("span rows = %d, want 1", n)
	}
	var data string
	mustNoErr(t, db.QueryRow(`SELECT data FROM spans WHERE id = 'sp1'`).Scan(&data))
	var got InferenceSpan
	mustNoErr(t, json.Unmarshal([]byte(data), &got))
	if got.Status != "completed" || got.OutputTokens != 42 {
		t.Errorf("persisted span = %+v, want latest upsert", got)
	}
	var provider string
	mustNoErr(t, db.QueryRow(`SELECT json_extract(data, '$.provider') FROM sessions WHERE id = 's1'`).Scan(&provider))
	if provider != "claude_code" {
		t.Errorf("persisted session provider = %q, want claude_code", provider)
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func ids[T any](vs []T, key func(T) string) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = key(v)
	}
	return strings.Join(parts, ",")
}
