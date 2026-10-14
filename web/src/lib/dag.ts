import type { AgentNode, InferenceSpan, ToolInvocation } from "../types/telemetry";

export interface GraphNode {
  id: string;
  x: number;
  y: number;
  agent: AgentNode;
  lastSpan?: InferenceSpan;
  toolCount: number;
  pill: boolean; // Utility calls (title, compactor, other helpers) render as compact pills
}

export interface GraphEdge {
  id: string;
  source: string;
  target: string;
  label?: string; // The spawning Task/Agent call's description, once linked
  animated: boolean;
}

const COL = 300;
const ROW = 190;
const PILL_ROW = 64;

// buildGraph lays out a session's agents: main agents on the top row,
// subagents in a row beneath their parent, utility pills in a column to the
// left. Positions are deterministic so live updates do not shuffle nodes.
const shorten = (s: string, n: number) => (s.length > n ? `${s.slice(0, n - 1)}…` : s);

export function buildGraph(agents: AgentNode[], spans: InferenceSpan[], tools: ToolInvocation[]) {
  const lastSpan = new Map<string, InferenceSpan>();
  for (const sp of spans) lastSpan.set(sp.agent_id, sp); // spans are oldest first
  const toolCount = new Map<string, number>();
  for (const t of tools) toolCount.set(t.agent_id, (toolCount.get(t.agent_id) ?? 0) + 1);
  const toolById = new Map(tools.map((t) => [t.tool_use_id, t]));

  const node = (agent: AgentNode, x: number, y: number, pill: boolean): GraphNode => ({
    id: agent.id, x, y, agent, pill, lastSpan: lastSpan.get(agent.id), toolCount: toolCount.get(agent.id) ?? 0,
  });

  const mains = agents.filter((a) => a.role === "main");
  const subs = agents.filter((a) => a.role === "subagent");
  const pills = agents.filter((a) => a.role !== "main" && a.role !== "subagent");

  const nodes: GraphNode[] = [];
  const edges: GraphEdge[] = [];
  mains.forEach((a, i) => nodes.push(node(a, i * COL * 3, 0, false)));
  pills.forEach((a, i) => nodes.push(node(a, -COL, i * PILL_ROW, true)));

  const known = new Set(agents.map((a) => a.id));
  const byParent = new Map<string, AgentNode[]>();
  for (const a of subs) {
    const parent = a.parent_agent_id && known.has(a.parent_agent_id) ? a.parent_agent_id : (mains[0]?.id ?? "");
    byParent.set(parent, [...(byParent.get(parent) ?? []), a]);
  }
  for (const [parent, children] of byParent) {
    const p = nodes.find((n) => n.id === parent);
    const depth = p ? p.y / ROW + 1 : 1;
    children.forEach((a, i) => {
      nodes.push(node(a, (p?.x ?? 0) + (i - (children.length - 1) / 2) * COL, depth * ROW, false));
      if (!parent) return;
      const task = a.parent_tool_id ? toolById.get(a.parent_tool_id) : undefined;
      edges.push({
        id: `${parent}->${a.id}`,
        source: parent,
        target: a.id,
        label: a.parent_tool_id ? shorten(task?.target || task?.tool_name || "Task", 26) : undefined,
        animated: a.status === "streaming" || a.status === "running_tool",
      });
    });
  }
  for (const a of pills) {
    if (a.parent_agent_id && known.has(a.parent_agent_id)) {
      edges.push({ id: `${a.parent_agent_id}->${a.id}`, source: a.parent_agent_id, target: a.id, animated: a.status === "streaming" });
    }
  }
  return { nodes, edges };
}
