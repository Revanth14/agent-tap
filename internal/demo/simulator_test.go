package demo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Revanth14/agent-tap/internal/adapters/claudecode"
	"github.com/Revanth14/agent-tap/internal/telemetry"
)

// TestScenarioThroughTheRealRecorder plays the scenario once, fast, through
// the real proxy, parser and hook handler, and checks what the UI would show.
func TestScenarioThroughTheRealRecorder(t *testing.T) {
	store, err := telemetry.OpenStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := RunOnce(ctx, claudecode.NewRecorder(store, nil), Options{Speed: 200}); err != nil {
		t.Fatal(err)
	}

	// The response parsers finish just after the last response is read.
	var snap telemetry.StateSnapshot
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		snap = store.Snapshot()
		done := len(snap.Spans) > 0
		for _, sp := range snap.Spans {
			done = done && sp.CompletedAt != nil
		}
		if done && len(snap.Spans) == 16 || time.Now().After(deadline) {
			break
		}
	}

	if len(snap.Spans) != 16 {
		t.Fatalf("spans = %d, want 16 (title + 4 main + 3x3 subagent in VS Code, 2 in the CLI)", len(snap.Spans))
	}
	sessions := map[string]telemetry.Session{}
	for _, s := range snap.Sessions {
		sessions[s.ID] = s
	}
	if s := sessions["demo-vscode-1"]; s.PrimarySurface != telemetry.SurfaceVSCodeExt || s.WorkspacePath != workspace || s.MaxContextWindow != 1_000_000 {
		t.Errorf("VS Code session = %+v", s)
	}
	if s := sessions["demo-cli-1"]; s.PrimarySurface != telemetry.SurfaceTerminalCLI {
		t.Errorf("CLI session = %+v", s)
	}

	agentTools := map[string]bool{} // Agent tool_use IDs
	for _, tl := range snap.Tools {
		if tl.Status != "completed" {
			t.Errorf("tool %s (%s) = %q, want completed", tl.ToolUseID, tl.ToolName, tl.Status)
		}
		if tl.DurationMs <= 0 {
			t.Errorf("tool %s (%s) has no duration", tl.ToolUseID, tl.ToolName)
		}
		if tl.ToolName == "Agent" {
			agentTools[tl.ToolUseID] = true
		}
	}
	if len(snap.Tools) != 11 || len(agentTools) != 3 {
		t.Errorf("tools = %d with %d Agent calls, want 11 (3 Agent, 6 subagent Grep/Read, Edit, Bash) with 3", len(snap.Tools), len(agentTools))
	}

	var subagents, titles int
	linked := map[string]bool{}
	for _, a := range snap.Agents {
		switch a.Role {
		case telemetry.RoleSubagent:
			subagents++
			if a.Status != "completed" || a.SubagentType != "Explore" || a.Model != fastModel || a.Label == "" || a.ParentAgentID != "demo-vscode-1/main" {
				t.Errorf("subagent = %+v", a)
			}
			if !agentTools[a.ParentToolID] || linked[a.ParentToolID] {
				t.Errorf("subagent %s ParentToolID %q: want a distinct Agent call", a.ID, a.ParentToolID)
			}
			linked[a.ParentToolID] = true
		case telemetry.RoleBackgroundTitle:
			titles++
		}
	}
	if subagents != 3 || titles != 1 {
		t.Errorf("subagents = %d, title nodes = %d; want 3 and 1", subagents, titles)
	}

	var ideBlocks, invalidated int
	for _, sp := range snap.Spans {
		if sp.Status != "completed" {
			t.Errorf("span %s = %q, want completed", sp.ID, sp.Status)
		}
		if strings.HasPrefix(sp.SessionID, "demo-vscode") && sp.Role == telemetry.RoleMain && len(sp.InjectedIDEBlocks) > ideBlocks {
			ideBlocks = len(sp.InjectedIDEBlocks)
		}
		if sp.CacheInvalidated {
			invalidated++
			if sp.SessionID != "demo-cli-1" {
				t.Errorf("cache invalidated in %s, want only the CLI session's second turn", sp.SessionID)
			}
		}
	}
	if ideBlocks != 2 || invalidated != 1 {
		t.Errorf("IDE blocks on main spans = %d, invalidated spans = %d; want 2 and 1", ideBlocks, invalidated)
	}
	if snap.PlanUsage != nil {
		t.Errorf("PlanUsage = %+v, want none: the demo must not fake account-level usage", snap.PlanUsage)
	}
}
