import type { AgentNode, InferenceSpan, PlanUsage, Session, ToolInvocation, WSEvent } from "../types/telemetry";

// The client keeps everything it has seen since its last snapshot, capped so
// a tab left open for days stays small. The daemon holds the history.
export const MAX_SPANS = 2000;
export const MAX_TOOLS = 4000;

export interface TelemetryState {
  sessions: Record<string, Session>;
  agents: Record<string, AgentNode>;
  spans: Record<string, InferenceSpan>;
  tools: Record<string, ToolInvocation>;
  planUsage: PlanUsage | null;
}

export const emptyState: TelemetryState = { sessions: {}, agents: {}, spans: {}, tools: {}, planUsage: null };

const byId = <T,>(items: T[] | null | undefined, id: (t: T) => string): Record<string, T> =>
  Object.fromEntries((items ?? []).map((t) => [id(t), t]));

// applyEvent folds one hub event into the state. A snapshot replaces it
// whole: the hub sends one on every (re)connect, and every later event
// upserts a record by ID.
export function applyEvent(s: TelemetryState, ev: WSEvent): TelemetryState {
  switch (ev.type) {
    case "snapshot": {
      const p = ev.payload;
      return {
        sessions: byId(p.sessions, (x) => x.id),
        agents: byId(p.agents, (x) => x.id),
        spans: byId(p.spans, (x) => x.id),
        tools: byId(p.tools, (x) => x.tool_use_id),
        planUsage: p.plan_usage,
      };
    }
    case "session.updated":
      return { ...s, sessions: { ...s.sessions, [ev.payload.id]: ev.payload } };
    case "agent.upserted":
      return { ...s, agents: { ...s.agents, [ev.payload.id]: ev.payload } };
    case "span.started":
    case "span.delta":
    case "span.completed": {
      const prev = s.spans[ev.payload.id];
      if (prev?.completed_at && !ev.payload.completed_at) return s; // Never reopen a finished span.
      return { ...s, spans: cap({ ...s.spans, [ev.payload.id]: ev.payload }, MAX_SPANS) };
    }
    case "tool.updated":
      return { ...s, tools: cap({ ...s.tools, [ev.payload.tool_use_id]: ev.payload }, MAX_TOOLS) };
    case "usage.updated":
      return { ...s, planUsage: ev.payload };
  }
  return s;
}

// cap keeps the newest records by started_at once the map grows 10% past max.
function cap<T extends { started_at: string }>(m: Record<string, T>, max: number): Record<string, T> {
  const keys = Object.keys(m);
  if (keys.length <= max * 1.1) return m;
  const newest = keys.sort((a, b) => m[b]!.started_at.localeCompare(m[a]!.started_at)).slice(0, max);
  return Object.fromEntries(newest.map((k) => [k, m[k]!]));
}

export interface SessionView {
  session: Session;
  agents: AgentNode[];
  spans: InferenceSpan[]; // Oldest first
  tools: ToolInvocation[];
}

const byStart = (a: { started_at: string }, b: { started_at: string }) => a.started_at.localeCompare(b.started_at);

export function sessionsByRecency(s: TelemetryState): Session[] {
  return Object.values(s.sessions).sort((a, b) => b.updated_at.localeCompare(a.updated_at));
}

export function sessionView(s: TelemetryState, id: string): SessionView | null {
  const session = s.sessions[id];
  if (!session) return null;
  const agents = Object.values(s.agents).filter((a) => a.session_id === id).sort(byStart);
  const agentIds = new Set(agents.map((a) => a.id));
  return {
    session,
    agents,
    spans: Object.values(s.spans).filter((sp) => sp.session_id === id).sort(byStart),
    tools: Object.values(s.tools).filter((t) => agentIds.has(t.agent_id)).sort(byStart),
  };
}
