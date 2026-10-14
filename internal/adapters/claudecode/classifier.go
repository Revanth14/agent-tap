package claudecode

import (
	"bytes"
	"cmp"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Revanth14/agent-tap/internal/telemetry"
)

const (
	previewRunes  = 300
	context1MBeta = "context-1m-2025-08-07"
)

var (
	ideTag  = regexp.MustCompile(`(?s)<(ide_opened_file|ide_selection|ide_diagnostics)>(.*?)</(?:ide_opened_file|ide_selection|ide_diagnostics)>`)
	idePath = regexp.MustCompile(`/[^\s:<>"'()]+`)
)

// requestInfo is what one /v1/messages request says about who sent it and why.
type requestInfo struct {
	SessionID       string
	AgentID         string
	ParentAgentID   string              // Empty for the session's main agent
	AgentRole       telemetry.AgentRole // Role of the agent node the request belongs to
	SubagentType    string
	Role            telemetry.AgentRole // Role of this request
	Surface         telemetry.SurfaceType
	Model           string
	LongContext     bool // Per-request 1M context opt-in
	ToolsOffered    []string
	SystemPreview   string
	LastUserPreview string
	FirstUserText   string // Full first user message, kept for subagents only (Task linking)
	ToolResults     []toolResult
	IDEBlocks       []telemetry.IDEContextBlock
}

// toolResult reports a tool call's outcome, carried in the request that
// follows it.
type toolResult struct {
	ToolUseID string
	IsError   bool
}

type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
}

type messagesRequest struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// classify reads a request's x-claude-code-* headers and User-Agent first,
// and its body for previews and the fallback rules (SPEC section 4.2). A
// malformed body still yields the header-based fields.
func classify(h http.Header, body []byte) requestInfo {
	var req messagesRequest
	_ = json.Unmarshal(body, &req)
	system := blockText(req.System)
	// Claude Code also sends role "system" messages mid-conversation; they
	// occupy context too, so they are scanned for IDE blocks.
	userTexts := make([]string, len(req.Messages)) // Empty for non-user messages
	contextTexts := []string{system}
	lastAssistant := -1
	for i, m := range req.Messages {
		switch m.Role {
		case "user":
			userTexts[i] = blockText(m.Content)
			contextTexts = append(contextTexts, userTexts[i])
		case "system":
			contextTexts = append(contextTexts, blockText(m.Content))
		case "assistant":
			lastAssistant = i
		}
	}
	var lastUser string
	for i := len(userTexts) - 1; i >= 0 && lastUser == ""; i-- {
		lastUser = userTexts[i]
	}

	info := requestInfo{
		SessionID:       h.Get("X-Claude-Code-Session-Id"),
		SubagentType:    h.Get("X-Claude-Code-Agent-Type"),
		Model:           req.Model,
		LongContext:     hasBetaFlag(h.Values("Anthropic-Beta"), context1MBeta),
		ToolsOffered:    make([]string, 0, len(req.Tools)),
		SystemPreview:   truncateRunes(system, previewRunes),
		LastUserPreview: truncateRunes(lastUser, previewRunes),
	}
	for _, t := range req.Tools {
		info.ToolsOffered = append(info.ToolsOffered, t.Name)
	}
	// New tool results are in the user messages after the last assistant
	// message; a system message may follow them.
	for _, m := range req.Messages[lastAssistant+1:] {
		if m.Role == "user" {
			info.ToolResults = append(info.ToolResults, toolResults(m.Content)...)
		}
	}
	info.IDEBlocks = ideBlocks(contextTexts)
	info.Role = requestRole(h.Get("X-Claude-Code-Request-Class"), info.ToolsOffered, system, lastUser)
	info.Surface = surface(h.Get("User-Agent"), body)

	// A request without an agent ID belongs to a per-session node for its
	// role: the main agent, or a utility node (title, compactor) under it.
	mainID := info.SessionID + "/main"
	if id := h.Get("X-Claude-Code-Agent-Id"); id != "" {
		info.AgentID, info.AgentRole = id, telemetry.RoleSubagent
		info.ParentAgentID = cmp.Or(h.Get("X-Claude-Code-Parent-Agent-Id"), mainID)
		if len(userTexts) > 0 {
			info.FirstUserText = userTexts[0]
		}
	} else {
		info.AgentID, info.AgentRole = info.SessionID+"/"+string(info.Role), info.Role
		if info.Role != telemetry.RoleMain {
			info.ParentAgentID = mainID
		}
	}
	return info
}

func requestRole(class string, tools []string, system, lastUser string) telemetry.AgentRole {
	switch class {
	case "main":
		return telemetry.RoleMain
	case "subagent", "workflow":
		return telemetry.RoleSubagent
	case "compaction":
		return telemetry.RoleCompactor
	case "auxiliary":
		// Title calls carry no tools. Other auxiliary calls can reuse the main
		// system prompt, which itself mentions titles and topics.
		if len(tools) == 0 && containsAny(strings.ToLower(system+"\n"+lastUser), "title", "5-word", "topic") {
			return telemetry.RoleBackgroundTitle
		}
		return telemetry.RoleFastUtility
	}

	// Fallback for requests without a known request class.
	hasAll := func(names ...string) bool {
		return !slices.ContainsFunc(names, func(n string) bool { return !slices.Contains(tools, n) })
	}
	switch {
	case slices.Contains(tools, "Task") || slices.Contains(tools, "Agent") || len(tools) >= 8 && hasAll("Edit", "Write", "Bash"):
		return telemetry.RoleMain
	case len(tools) > 0:
		return telemetry.RoleSubagent
	}
	text := strings.ToLower(system + "\n" + lastUser)
	switch {
	case containsAny(text, "title", "5-word", "topic"):
		return telemetry.RoleBackgroundTitle
	case containsAny(text, "compact", "summarize the conversation", "checkpoint"):
		return telemetry.RoleCompactor
	}
	return telemetry.RoleFastUtility
}

// surface reads the entrypoint from a User-Agent such as
// "claude-cli/2.1.299 (external, claude-vscode, agent-sdk/0.3.299)", falling
// back to the presence of VS Code's <ide_*> tags.
func surface(userAgent string, body []byte) telemetry.SurfaceType {
	if _, rest, ok := strings.Cut(userAgent, "(external, "); ok {
		entry, _, _ := strings.Cut(rest, ",")
		entry, _, _ = strings.Cut(entry, ")")
		switch strings.TrimSpace(entry) {
		case "claude-vscode":
			return telemetry.SurfaceVSCodeExt
		case "cli", "sdk-cli":
			return telemetry.SurfaceTerminalCLI
		}
	}
	if bytes.Contains(body, []byte("<ide_")) {
		return telemetry.SurfaceVSCodeExt
	}
	return telemetry.SurfaceTerminalCLI
}

// blockText returns the text of a content field that is either a string or
// an array of blocks, joining the text blocks.
func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type != "text" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(bl.Text)
	}
	return b.String()
}

func toolResults(raw json.RawMessage) []toolResult {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []toolResult
	for _, b := range blocks {
		if b.Type == "tool_result" && b.ToolUseID != "" {
			out = append(out, toolResult{ToolUseID: b.ToolUseID, IsError: b.IsError})
		}
	}
	return out
}

// ideBlocks extracts the <ide_*> context blocks VS Code injects, across the
// whole request: every one of them occupies context.
func ideBlocks(texts []string) []telemetry.IDEContextBlock {
	var out []telemetry.IDEContextBlock
	for _, text := range texts {
		for _, m := range ideTag.FindAllStringSubmatch(text, -1) {
			inner := strings.TrimSpace(m[2])
			n := utf8.RuneCountInString(inner)
			out = append(out, telemetry.IDEContextBlock{
				TagType:   m[1],
				FilePath:  idePath.FindString(inner),
				CharLen:   n,
				EstTokens: n / 4,
				Preview:   truncateRunes(inner, previewRunes),
			})
		}
	}
	return out
}

func hasBetaFlag(values []string, flag string) bool {
	for _, v := range values {
		for f := range strings.SplitSeq(v, ",") {
			if strings.TrimSpace(f) == flag {
				return true
			}
		}
	}
	return false
}

func containsAny(s string, subs ...string) bool {
	return slices.ContainsFunc(subs, func(sub string) bool { return strings.Contains(s, sub) })
}

func truncateRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}
