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

func TestStorePruneDeletesOldRowsFromEveryTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(path, 0)
	mustNoErr(t, err)
	now := time.Now()
	for _, r := range []struct {
		prefix  string
		started time.Time
	}{{"old", now.Add(-31 * 24 * time.Hour)}, {"new", now.Add(-29 * 24 * time.Hour)}} {
		sess := r.prefix + "-s"
		mustNoErr(t, s.UpsertSession(Session{ID: sess, StartedAt: r.started}))
		mustNoErr(t, s.UpsertAgent(AgentNode{ID: r.prefix + "-a", SessionID: sess, StartedAt: r.started}))
		mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: r.prefix + "-sp", SessionID: sess, AgentID: r.prefix + "-a", StartedAt: r.started}))
		mustNoErr(t, s.UpsertTool(ToolInvocation{ToolUseID: r.prefix + "-t", SpanID: r.prefix + "-sp", StartedAt: r.started}))
	}

	n, err := s.Prune(now.Add(-30 * 24 * time.Hour))
	mustNoErr(t, err)
	if n != 4 {
		t.Errorf("pruned %d rows, want 4 (one per table)", n)
	}
	mustNoErr(t, s.Close())

	db, err := sql.Open("sqlite", path)
	mustNoErr(t, err)
	defer db.Close()
	for table, idCol := range map[string]string{"sessions": "id", "agents": "id", "spans": "id", "tools": "tool_use_id"} {
		var ids []string
		rows, err := db.Query(`SELECT ` + idCol + ` FROM ` + table)
		mustNoErr(t, err)
		for rows.Next() {
			var id string
			mustNoErr(t, rows.Scan(&id))
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) != 1 || !strings.HasPrefix(ids[0], "new-") {
			t.Errorf("%s after prune = %v, want only the row from 29 days ago", table, ids)
		}
	}

	mem, err := OpenStore("", 0)
	mustNoErr(t, err)
	if n, err := mem.Prune(now); n != 0 || err != nil {
		t.Errorf("memory-only Prune = %d, %v; want 0, nil", n, err)
	}
}

func TestStoreLoadRestoresRecentSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(path, 2)
	mustNoErr(t, err)
	t0 := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	done := t0.Add(time.Second)
	// One more session than the store keeps live; s00 was updated longest ago.
	for i := range MaxLiveSessions + 1 {
		id := fmt.Sprintf("s%02d", i)
		mustNoErr(t, s.UpsertSession(Session{ID: id, StartedAt: t0, UpdatedAt: t0.Add(time.Duration(i) * time.Minute), TotalOutputTokens: 100 + i}))
	}
	mustNoErr(t, s.UpsertAgent(AgentNode{ID: "s16/main", SessionID: "s16", Status: "streaming", ActiveTool: "Bash"}))
	for i, id := range []string{"sp1", "sp2", "sp3"} { // ring capacity is 2
		mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: id, SessionID: "s16", AgentID: "s16/main", StartedAt: t0.Add(time.Duration(i) * time.Second), Status: "completed", CompletedAt: &done}))
	}
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "sp-inflight", SessionID: "s16", AgentID: "s16/main", StartedAt: t0.Add(time.Hour), Status: "decoding"}))
	mustNoErr(t, s.UpsertTool(ToolInvocation{ToolUseID: "tu1", SpanID: "sp3", AgentID: "s16/main", StartedAt: t0}))
	mustNoErr(t, s.Close())

	s, err = OpenStore(path, 2)
	mustNoErr(t, err)
	defer s.Close()
	if n := len(s.Snapshot().Sessions); n != 0 {
		t.Fatalf("sessions before Load = %d, want 0", n)
	}
	snap, err := s.Load()
	mustNoErr(t, err)

	sessions := "," + ids(snap.Sessions, func(v Session) string { return v.ID }) + ","
	if len(snap.Sessions) != MaxLiveSessions || strings.Contains(sessions, ",s00,") || !strings.Contains(sessions, ",s16,") {
		t.Errorf("loaded sessions = %s, want the %d most recently updated", sessions, MaxLiveSessions)
	}
	if got := ids(snap.Spans, func(v InferenceSpan) string { return v.ID + ":" + v.Status }); got != "sp3:completed,sp-inflight:error" {
		t.Errorf("spans = %s, want the newest 2, the in-flight one marked an error", got)
	}
	for _, sp := range snap.Spans {
		if sp.ID == "sp-inflight" && (sp.StopReason != "daemon_restarted" || sp.CompletedAt == nil) {
			t.Errorf("in-flight span = %+v, want daemon_restarted with CompletedAt", sp)
		}
	}
	if len(snap.Agents) != 1 || snap.Agents[0].Status != "idle" || snap.Agents[0].ActiveTool != "" {
		t.Errorf("agents = %+v, want s16/main idle", snap.Agents)
	}
	if len(snap.Tools) != 1 {
		t.Errorf("tools = %+v, want tu1", snap.Tools)
	}

	// The repairs were written back, so a second restart sees them too.
	mustNoErr(t, s.Close())
	s, err = OpenStore(path, 2)
	mustNoErr(t, err)
	snap, err = s.Load()
	mustNoErr(t, err)
	if got := ids(snap.Spans, func(v InferenceSpan) string { return v.ID + ":" + v.StopReason }); got != "sp3:,sp-inflight:daemon_restarted" {
		t.Errorf("spans after a second restart = %s", got)
	}
}

// A tool whose arguments were still streaming when the daemon stopped can
// never run: Claude Code saw the stream break. Load fails it, as spanFailed
// would have, instead of leaving it streaming forever.
func TestStoreLoadFailsToolsOfInterruptedSpans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(path, 0)
	mustNoErr(t, err)
	t0 := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	mustNoErr(t, s.UpsertSession(Session{ID: "s", StartedAt: t0, UpdatedAt: t0}))
	mustNoErr(t, s.UpsertAgent(AgentNode{ID: "s/main", SessionID: "s", Status: "streaming"}))
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "sp", SessionID: "s", AgentID: "s/main", StartedAt: t0, Status: "decoding"}))
	mustNoErr(t, s.UpsertTool(ToolInvocation{ToolUseID: "tu-args", SpanID: "sp", AgentID: "s/main", StartedAt: t0, Status: "streaming_args"}))
	mustNoErr(t, s.Close())

	for restart := range 2 { // The repair is written back, so a second restart sees it too.
		s, err = OpenStore(path, 0)
		mustNoErr(t, err)
		snap, err := s.Load()
		mustNoErr(t, err)
		if len(snap.Tools) != 1 || snap.Tools[0].Status != "failed" {
			t.Errorf("restart %d: tools = %+v, want tu-args failed", restart+1, snap.Tools)
		}
		mustNoErr(t, s.Close())
	}
}
