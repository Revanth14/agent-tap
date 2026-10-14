// TypeScript mirror of internal/telemetry/schema.go. Keep in sync.

export type SurfaceType = "vscode_ext" | "terminal_cli" | "unknown";

export type AgentRole = "main" | "subagent" | "background_title" | "compactor" | "fast_utility";

export interface Session {
  id: string;
  provider: string;
  workspace_path: string;
  primary_surface: SurfaceType;
  started_at: string;
  updated_at: string;
  total_input_tokens: number;
  total_output_tokens: number;
  total_cache_read: number;
  total_cache_write: number;
  current_context_size: number;
  max_context_window: number;
}

export interface AgentNode {
  id: string;
  session_id: string;
  parent_agent_id?: string;
  parent_tool_id?: string;
  role: AgentRole;
  subagent_type?: string;
  label: string;
  model: string;
  surface: SurfaceType;
  status: "streaming" | "running_tool" | "idle" | "completed" | "error";
  active_tool?: string;
  started_at: string;
  completed_at?: string;
  total_tokens: number;
}

export interface IDEContextBlock {
  tag_type: "ide_opened_file" | "ide_selection" | "ide_diagnostics";
  file_path?: string;
  char_len: number;
  est_tokens: number;
  preview: string;
}

export interface InferenceSpan {
  id: string;
  session_id: string;
  agent_id: string;
  surface: SurfaceType;
  role: AgentRole;
  model: string;
  status: "prefill" | "decoding" | "completed" | "error";
  started_at: string;
  first_token_at?: string;
  completed_at?: string;
  ttft_ms: number;
  duration_ms: number;
  tokens_per_sec: number;
  input_tokens_uncached: number;
  input_tokens_cache_write: number;
  input_tokens_cache_read: number;
  output_tokens: number;
  cache_hit_ratio: number;
  cache_invalidated: boolean;
  stop_reason?: string;
  tools_offered: string[] | null;
  tools_invoked?: ToolInvocation[];
  injected_ide_blocks?: IDEContextBlock[];
  system_prompt_preview: string;
  last_user_message_preview: string;
  assistant_output_preview: string;
}

export interface ToolInvocation {
  tool_use_id: string;
  span_id: string;
  agent_id: string;
  tool_name: string;
  target: string;
  status: "streaming_args" | "running" | "completed" | "failed";
  started_at: string;
  duration_ms?: number;
}

export interface UsageWindow {
  name: string;
  utilization: number; // 0-1
  resets_at?: string;
  status: string;
}

export interface PlanUsage {
  provider: string;
  windows: UsageWindow[];
  observed_at: string;
}

export interface StateSnapshot {
  sessions: Session[];
  agents: AgentNode[];
  spans: InferenceSpan[];
  tools: ToolInvocation[];
  plan_usage: PlanUsage | null;
}

export type WSEvent =
  | { type: "snapshot"; timestamp: string; payload: StateSnapshot }
  | { type: "session.updated"; timestamp: string; payload: Session }
  | { type: "agent.upserted"; timestamp: string; payload: AgentNode }
  | { type: "span.started" | "span.delta" | "span.completed"; timestamp: string; payload: InferenceSpan }
  | { type: "tool.updated"; timestamp: string; payload: ToolInvocation }
  | { type: "usage.updated"; timestamp: string; payload: PlanUsage };
