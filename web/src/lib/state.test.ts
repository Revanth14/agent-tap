import { describe, expect, it } from "vitest";
import { MAX_SPANS, applyEvent, emptyState, sessionView } from "./state";
import type { InferenceSpan, StateSnapshot } from "../types/telemetry";

const span = (id: string, over: Partial<InferenceSpan> = {}): InferenceSpan => ({
  id, session_id: "s", agent_id: "s/main", surface: "terminal_cli", role: "main", model: "claude-opus-5-5",
  status: "decoding", started_at: "2026-10-14T10:00:00Z", ttft_ms: 0, duration_ms: 0, tokens_per_sec: 0,
  input_tokens_uncached: 0, input_tokens_cache_write: 0, input_tokens_cache_read: 0, output_tokens: 0,
  cache_hit_ratio: 0, cache_invalidated: false, tools_offered: [], system_prompt_preview: "",
  last_user_message_preview: "", assistant_output_preview: "", ...over,
});

const snapshot = (over: Partial<StateSnapshot> = {}): StateSnapshot => ({
  sessions: [], agents: [], spans: [], tools: [], plan_usage: null, ...over,
});

describe("applyEvent", () => {
  it("replaces all state on a snapshot, so a reconnect cannot leave stale records", () => {
    let s = applyEvent(emptyState, { type: "snapshot", timestamp: "", payload: snapshot({ spans: [span("old")] }) });
    s = applyEvent(s, { type: "snapshot", timestamp: "", payload: snapshot({ spans: [span("new")] }) });
    expect(Object.keys(s.spans)).toEqual(["new"]);
  });

  it("tolerates null slices in a snapshot", () => {
    const s = applyEvent(emptyState, { type: "snapshot", timestamp: "", payload: { ...snapshot(), tools: null as never } });
    expect(s.tools).toEqual({});
  });

  it("upserts spans by ID and never reopens a finished one", () => {
    let s = applyEvent(emptyState, { type: "span.started", timestamp: "", payload: span("a") });
    s = applyEvent(s, { type: "span.completed", timestamp: "", payload: span("a", { status: "completed", completed_at: "2026-10-14T10:00:05Z" }) });
    s = applyEvent(s, { type: "span.delta", timestamp: "", payload: span("a", { status: "decoding" }) });
    expect(s.spans.a?.status).toBe("completed");
  });

  it("keeps the newest spans once the cap is exceeded", () => {
    let s = emptyState;
    const n = Math.ceil(MAX_SPANS * 1.1) + 1;
    for (let i = 0; i < n; i++) {
      const t = new Date(Date.UTC(2026, 9, 14, 0, 0, i)).toISOString();
      s = applyEvent(s, { type: "span.started", timestamp: "", payload: span(`sp${i}`, { started_at: t }) });
    }
    expect(Object.keys(s.spans)).toHaveLength(MAX_SPANS);
    expect(s.spans.sp0).toBeUndefined();
    expect(s.spans[`sp${n - 1}`]).toBeDefined();
  });

  it("replaces plan usage on usage.updated", () => {
    const usage = { provider: "claude_code", windows: [{ name: "5h", utilization: 0.7, status: "allowed" }], observed_at: "" };
    expect(applyEvent(emptyState, { type: "usage.updated", timestamp: "", payload: usage }).planUsage).toBe(usage);
  });
});

describe("sessionView", () => {
  it("collects a session's records, oldest first, and its agents' tools", () => {
    const base = { session_id: "s", role: "main" as const, label: "", model: "", surface: "unknown" as const, status: "idle" as const, total_tokens: 0 };
    const s = applyEvent(emptyState, {
      type: "snapshot", timestamp: "", payload: snapshot({
        sessions: [{ id: "s", provider: "", workspace_path: "", primary_surface: "unknown", started_at: "", updated_at: "",
          total_input_tokens: 0, total_output_tokens: 0, total_cache_read: 0, total_cache_write: 0, current_context_size: 0, max_context_window: 0 }],
        agents: [{ ...base, id: "s/main", started_at: "2" }, { ...base, id: "other", session_id: "x", started_at: "1" }],
        spans: [span("b", { started_at: "2" }), span("a", { started_at: "1" }), span("x", { session_id: "x" })],
        tools: [
          { tool_use_id: "t1", span_id: "a", agent_id: "s/main", tool_name: "Read", target: "", status: "completed", started_at: "1" },
          { tool_use_id: "t2", span_id: "x", agent_id: "other", tool_name: "Read", target: "", status: "completed", started_at: "1" },
        ],
      }),
    });
    const v = sessionView(s, "s")!;
    expect(v.spans.map((x) => x.id)).toEqual(["a", "b"]);
    expect(v.agents.map((x) => x.id)).toEqual(["s/main"]);
    expect(v.tools.map((x) => x.tool_use_id)).toEqual(["t1"]);
    expect(sessionView(s, "missing")).toBeNull();
  });
});
