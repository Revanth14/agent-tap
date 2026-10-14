package claudecode

import (
	"net/http"
	"slices"
	"testing"

	"github.com/Revanth14/agent-tap/internal/telemetry"
)

func TestClassify(t *testing.T) {
	const (
		cliUA    = "claude-cli/2.1.296 (external, cli)"
		vscodeUA = "claude-cli/2.1.299 (external, claude-vscode, agent-sdk/0.3.299)"
		sdkUA    = "claude-cli/2.1.299 (external, sdk-cli)"
	)
	body := func(system, user string, tools ...string) string {
		b := `{"model":"claude-opus-5-5","system":"` + system + `","messages":[{"role":"user","content":"` + user + `"}],"tools":[`
		for i, tl := range tools {
			if i > 0 {
				b += ","
			}
			b += `{"name":"` + tl + `"}`
		}
		return b + `]}`
	}
	tests := []struct {
		name    string
		headers map[string]string
		body    string
		want    requestInfo // only the fields below are compared
	}{
		{
			name:    "main turn from the CLI",
			headers: map[string]string{"User-Agent": cliUA, "X-Claude-Code-Session-Id": "s", "X-Claude-Code-Request-Class": "main"},
			body:    body("sys", "hi", "Read", "Agent"),
			want:    requestInfo{AgentID: "s/main", AgentRole: telemetry.RoleMain, Role: telemetry.RoleMain, Surface: telemetry.SurfaceTerminalCLI},
		},
		{
			name:    "subagent from VS Code: parent defaults to the main agent",
			headers: map[string]string{"User-Agent": vscodeUA, "X-Claude-Code-Session-Id": "s", "X-Claude-Code-Request-Class": "subagent", "X-Claude-Code-Agent-Id": "a1", "X-Claude-Code-Agent-Type": "Explore"},
			body:    body("sys", "find x", "Read", "Grep"),
			want:    requestInfo{AgentID: "a1", ParentAgentID: "s/main", AgentRole: telemetry.RoleSubagent, SubagentType: "Explore", Role: telemetry.RoleSubagent, Surface: telemetry.SurfaceVSCodeExt},
		},
		{
			name:    "explicit parent agent header wins",
			headers: map[string]string{"X-Claude-Code-Session-Id": "s", "X-Claude-Code-Request-Class": "subagent", "X-Claude-Code-Agent-Id": "a2", "X-Claude-Code-Parent-Agent-Id": "a1"},
			body:    body("sys", "x", "Read"),
			want:    requestInfo{AgentID: "a2", ParentAgentID: "a1", AgentRole: telemetry.RoleSubagent, Role: telemetry.RoleSubagent, Surface: telemetry.SurfaceTerminalCLI},
		},
		{
			name:    "auxiliary title call gets a title node under main",
			headers: map[string]string{"User-Agent": sdkUA, "X-Claude-Code-Session-Id": "s", "X-Claude-Code-Request-Class": "auxiliary"},
			body:    body("Generate a 5-word title for this conversation", "hi"),
			want:    requestInfo{AgentID: "s/background_title", ParentAgentID: "s/main", AgentRole: telemetry.RoleBackgroundTitle, Role: telemetry.RoleBackgroundTitle, Surface: telemetry.SurfaceTerminalCLI},
		},
		{
			name:    "auxiliary call with tools is not a title call, even if its system prompt mentions titles",
			headers: map[string]string{"X-Claude-Code-Session-Id": "s", "X-Claude-Code-Request-Class": "auxiliary"},
			body:    body("You are Claude Code. Use a short title for commits. Stay on topic.", "hi", "Read", "Agent"),
			want:    requestInfo{AgentID: "s/fast_utility", ParentAgentID: "s/main", AgentRole: telemetry.RoleFastUtility, Role: telemetry.RoleFastUtility, Surface: telemetry.SurfaceTerminalCLI},
		},
		{
			name:    "auxiliary call on a subagent's behalf belongs to that subagent",
			headers: map[string]string{"X-Claude-Code-Session-Id": "s", "X-Claude-Code-Request-Class": "auxiliary", "X-Claude-Code-Agent-Id": "a1"},
			body:    body("Summarize progress", "x"),
			want:    requestInfo{AgentID: "a1", ParentAgentID: "s/main", AgentRole: telemetry.RoleSubagent, Role: telemetry.RoleFastUtility, Surface: telemetry.SurfaceTerminalCLI},
		},
		{
			name:    "compaction",
			headers: map[string]string{"X-Claude-Code-Session-Id": "s", "X-Claude-Code-Request-Class": "compaction", "X-Claude-Code-Compaction": "manual"},
			body:    body("sys", "x"),
			want:    requestInfo{AgentID: "s/compactor", ParentAgentID: "s/main", AgentRole: telemetry.RoleCompactor, Role: telemetry.RoleCompactor, Surface: telemetry.SurfaceTerminalCLI},
		},
		{
			name:    "fallback without request class: Agent tool means main",
			headers: map[string]string{"X-Claude-Code-Session-Id": "s"},
			body:    body("sys", "x", "Read", "Agent"),
			want:    requestInfo{AgentID: "s/main", AgentRole: telemetry.RoleMain, Role: telemetry.RoleMain, Surface: telemetry.SurfaceTerminalCLI},
		},
		{
			name:    "fallback: tools without Agent means subagent",
			headers: map[string]string{"X-Claude-Code-Session-Id": "s"},
			body:    body("sys", "x", "Read", "Grep"),
			want:    requestInfo{AgentID: "s/subagent", ParentAgentID: "s/main", AgentRole: telemetry.RoleSubagent, Role: telemetry.RoleSubagent, Surface: telemetry.SurfaceTerminalCLI},
		},
		{
			name:    "fallback: no tools and compaction text",
			headers: map[string]string{"X-Claude-Code-Session-Id": "s"},
			body:    body("Please summarize the conversation so far", "x"),
			want:    requestInfo{AgentID: "s/compactor", ParentAgentID: "s/main", AgentRole: telemetry.RoleCompactor, Role: telemetry.RoleCompactor, Surface: telemetry.SurfaceTerminalCLI},
		},
		{
			name:    "fallback surface: <ide_*> tag without a known User-Agent",
			headers: map[string]string{"X-Claude-Code-Session-Id": "s", "X-Claude-Code-Request-Class": "main"},
			body:    body("sys", "<ide_opened_file>a.go</ide_opened_file> hi", "Agent"),
			want:    requestInfo{AgentID: "s/main", AgentRole: telemetry.RoleMain, Role: telemetry.RoleMain, Surface: telemetry.SurfaceVSCodeExt},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			got := classify(h, []byte(tt.body))
			if got.AgentID != tt.want.AgentID || got.ParentAgentID != tt.want.ParentAgentID || got.AgentRole != tt.want.AgentRole ||
				got.SubagentType != tt.want.SubagentType || got.Role != tt.want.Role || got.Surface != tt.want.Surface {
				t.Errorf("classify =\n%+v\nwant (selected fields)\n%+v", got, tt.want)
			}
		})
	}
}

func TestClassifyLongContextAndPreviews(t *testing.T) {
	h := http.Header{}
	h.Add("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20, context-1m-2025-08-07 ,effort-2025-11-24")
	long := make([]rune, previewRunes+50)
	for i := range long {
		long[i] = 'é'
	}
	body := `{"model":"claude-opus-4-6","messages":[` +
		`{"role":"user","content":"first"},` +
		`{"role":"assistant","content":"ok"},` +
		`{"role":"user","content":[{"type":"text","text":"` + string(long) + `"}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]}]}`
	got := classify(h, []byte(body))
	if !got.LongContext {
		t.Error("LongContext = false, want true for the context-1m beta flag")
	}
	if got.LastUserPreview != string(long[:previewRunes]) {
		t.Errorf("LastUserPreview has %d runes, want the last user text cut to %d runes", len([]rune(got.LastUserPreview)), previewRunes)
	}
	if got.Model != "claude-opus-4-6" || got.ToolsOffered == nil {
		t.Errorf("Model %q, ToolsOffered %v; want claude-opus-4-6 and a non-nil slice", got.Model, got.ToolsOffered)
	}
	if classify(http.Header{"Anthropic-Beta": {"context-1m-2025-08-07-extra"}}, nil).LongContext {
		t.Error("LongContext = true for a different flag sharing the prefix")
	}
}

func TestClassifyIDEBlocksAndToolResults(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"<ide_opened_file>The user opened the file /repo/internal/store.go in the IDE. This may or may not be related.</ide_opened_file>"},` +
		`{"type":"text","text":"<ide_selection>The user selected the lines 3 to 5 from /repo/hub.go:\nfunc x() {}</ide_selection> explain"}]},` +
		`{"role":"assistant","content":"ok"},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"x"},{"type":"tool_result","tool_use_id":"toolu_2","is_error":true}]}]}`
	got := classify(http.Header{}, []byte(body))

	if len(got.IDEBlocks) != 2 {
		t.Fatalf("IDEBlocks = %+v, want 2", got.IDEBlocks)
	}
	if b := got.IDEBlocks[0]; b.TagType != "ide_opened_file" || b.FilePath != "/repo/internal/store.go" || b.CharLen != 92 || b.EstTokens != 23 {
		t.Errorf("opened-file block = %+v", b)
	}
	if b := got.IDEBlocks[1]; b.TagType != "ide_selection" || b.FilePath != "/repo/hub.go" {
		t.Errorf("selection block = %+v", b)
	}
	want := []toolResult{{ToolUseID: "toolu_1"}, {ToolUseID: "toolu_2", IsError: true}}
	if !slices.Equal(got.ToolResults, want) {
		t.Errorf("ToolResults = %+v, want %+v", got.ToolResults, want)
	}
	if got.FirstUserText != "" {
		t.Errorf("FirstUserText = %q, want empty for a main-agent request", got.FirstUserText)
	}
}
