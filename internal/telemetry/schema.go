// Package telemetry defines the provider-agnostic canonical telemetry schema
// shared by the Go daemon and the React UI, plus the model table, storage and
// the WebSocket hub built on it.
package telemetry

import "time"

type SurfaceType string

const (
	SurfaceVSCodeExt   SurfaceType = "vscode_ext"
	SurfaceTerminalCLI SurfaceType = "terminal_cli"
	SurfaceUnknown     SurfaceType = "unknown"
)

type AgentRole string

const (
	RoleMain            AgentRole = "main"             // Primary interactive coding agent
	RoleSubagent        AgentRole = "subagent"         // Spawned via Task/Agent tool (Explore, Plan, custom)
	RoleBackgroundTitle AgentRole = "background_title" // Haiku title / topic generator
	RoleCompactor       AgentRole = "compactor"        // Auto-compact or context summarizer
	RoleFastUtility     AgentRole = "fast_utility"     // Other lightweight helper call
)

type Session struct {
	ID                 string      `json:"id"`
	Provider           string      `json:"provider"` // "claude_code", "codex", "gemini", "cursor"
	WorkspacePath      string      `json:"workspace_path"`
	PrimarySurface     SurfaceType `json:"primary_surface"`
	StartedAt          time.Time   `json:"started_at"`
	UpdatedAt          time.Time   `json:"updated_at"`
	TotalInputTokens   int         `json:"total_input_tokens"`
	TotalOutputTokens  int         `json:"total_output_tokens"`
	TotalCacheRead     int         `json:"total_cache_read"`
	TotalCacheWrite    int         `json:"total_cache_write"`
	CurrentContextSize int         `json:"current_context_size"` // Latest Main Agent total input context
	MaxContextWindow   int         `json:"max_context_window"`   // e.g., 200000 or 1000000
}

type AgentNode struct {
	ID            string      `json:"id"`
	SessionID     string      `json:"session_id"`
	ParentAgentID string      `json:"parent_agent_id,omitempty"` // Empty if root Main Agent
	ParentToolID  string      `json:"parent_tool_id,omitempty"`  // ToolUse ID of the Task call that spawned this subagent
	Role          AgentRole   `json:"role"`
	SubagentType  string      `json:"subagent_type,omitempty"` // e.g., "Explore", "Plan", "general-purpose"
	Label         string      `json:"label"`                   // Short human description or task prompt snippet
	Model         string      `json:"model"`
	Surface       SurfaceType `json:"surface"`
	Status        string      `json:"status"`                // "streaming", "running_tool", "idle", "completed", "error"
	ActiveTool    string      `json:"active_tool,omitempty"` // Currently executing tool name + target
	StartedAt     time.Time   `json:"started_at"`
	CompletedAt   *time.Time  `json:"completed_at,omitempty"`
	TotalTokens   int         `json:"total_tokens"`
}

type IDEContextBlock struct {
	TagType   string `json:"tag_type"`            // "ide_opened_file", "ide_selection", "ide_diagnostics"
	FilePath  string `json:"file_path,omitempty"` // Extracted file path if present
	CharLen   int    `json:"char_len"`
	EstTokens int    `json:"est_tokens"` // CharLen / 4
	Preview   string `json:"preview"`    // First 300 chars
}

type InferenceSpan struct {
	ID                     string            `json:"id"`
	SessionID              string            `json:"session_id"`
	AgentID                string            `json:"agent_id"`
	Surface                SurfaceType       `json:"surface"`
	Role                   AgentRole         `json:"role"`
	Model                  string            `json:"model"`
	Status                 string            `json:"status"` // "prefill", "decoding", "completed", "error"
	StartedAt              time.Time         `json:"started_at"`
	FirstTokenAt           *time.Time        `json:"first_token_at,omitempty"`
	CompletedAt            *time.Time        `json:"completed_at,omitempty"`
	TTFTMs                 int64             `json:"ttft_ms"`
	DurationMs             int64             `json:"duration_ms"`
	TokensPerSec           float64           `json:"tokens_per_sec"`
	InputTokensUncached    int               `json:"input_tokens_uncached"`
	InputTokensCacheWrite  int               `json:"input_tokens_cache_write"`
	InputTokensCacheRead   int               `json:"input_tokens_cache_read"`
	OutputTokens           int               `json:"output_tokens"`
	CacheHitRatio          float64           `json:"cache_hit_ratio"`
	CacheInvalidated       bool              `json:"cache_invalidated"` // True if CacheRead dropped >50% vs prior main turn
	StopReason             string            `json:"stop_reason,omitempty"`
	ToolsOffered           []string          `json:"tools_offered"`
	ToolsInvoked           []ToolInvocation  `json:"tools_invoked,omitempty"`
	InjectedIDEBlocks      []IDEContextBlock `json:"injected_ide_blocks,omitempty"`
	SystemPromptPreview    string            `json:"system_prompt_preview"`
	LastUserMessagePreview string            `json:"last_user_message_preview"`
	AssistantOutputPreview string            `json:"assistant_output_preview"`
}

type ToolInvocation struct {
	ToolUseID  string    `json:"tool_use_id"`
	SpanID     string    `json:"span_id"`
	AgentID    string    `json:"agent_id"`
	ToolName   string    `json:"tool_name"` // "Read", "Edit", "Write", "Bash", "Grep", "Glob", "Task"
	Target     string    `json:"target"`    // Extracted file_path, command, pattern, or task description
	Status     string    `json:"status"`    // "streaming_args", "running", "completed", "failed"
	StartedAt  time.Time `json:"started_at"`
	DurationMs int64     `json:"duration_ms,omitempty"`
}

type StateSnapshot struct {
	Sessions []Session        `json:"sessions"`
	Agents   []AgentNode      `json:"agents"`
	Spans    []InferenceSpan  `json:"spans"`
	Tools    []ToolInvocation `json:"tools"`
}

type WSEvent struct {
	Type      string      `json:"type"` // "snapshot", "session.updated", "agent.upserted", "span.started", "span.delta", "span.completed", "tool.updated"
	Timestamp time.Time   `json:"timestamp"`
	Payload   interface{} `json:"payload"`
}
