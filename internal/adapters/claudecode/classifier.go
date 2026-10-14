package claudecode

import (
	"bytes"
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/Revanth14/agent-tap/internal/telemetry"
)

const (
	previewRunes  = 300
	context1MBeta = "context-1m-2025-08-07"
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
	var lastUser string
	for i := len(req.Messages) - 1; i >= 0 && lastUser == ""; i-- {
		if req.Messages[i].Role == "user" {
			lastUser = blockText(req.Messages[i].Content)
		}
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
	info.Role = requestRole(h.Get("X-Claude-Code-Request-Class"), info.ToolsOffered, system, lastUser)
	info.Surface = surface(h.Get("User-Agent"), body)

	// A request without an agent ID belongs to a per-session node for its
	// role: the main agent, or a utility node (title, compactor) under it.
	mainID := info.SessionID + "/main"
	if id := h.Get("X-Claude-Code-Agent-Id"); id != "" {
		info.AgentID, info.AgentRole = id, telemetry.RoleSubagent
		info.ParentAgentID = cmp.Or(h.Get("X-Claude-Code-Parent-Agent-Id"), mainID)
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
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
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
