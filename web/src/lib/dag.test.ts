import { describe, expect, it } from "vitest";
import { buildGraph } from "./dag";
import type { AgentNode, ToolInvocation } from "../types/telemetry";

const agent = (id: string, over: Partial<AgentNode> = {}): AgentNode => ({
  id, session_id: "s", role: "main", label: "", model: "", surface: "vscode_ext", status: "idle",
  started_at: "2026-10-14T10:00:00Z", total_tokens: 0, ...over,
});
const tool = (id: string, over: Partial<ToolInvocation> = {}): ToolInvocation => ({
  tool_use_id: id, span_id: "sp", agent_id: "s/main", tool_name: "Agent", target: "Find callers",
  status: "completed", started_at: "2026-10-14T10:00:00Z", ...over,
});

describe("buildGraph", () => {
  const agents = [
    agent("s/main"),
    agent("s/background_title", { role: "background_title", parent_agent_id: "s/main" }),
    agent("a1", { role: "subagent", parent_agent_id: "s/main", parent_tool_id: "toolu_1", status: "running_tool" }),
    agent("a2", { role: "subagent", parent_agent_id: "s/main" }), // Not linked to its Task call yet
  ];
  const { nodes, edges } = buildGraph(agents, [], [tool("toolu_1"), tool("t2", { agent_id: "a1", tool_name: "Grep" })]);

  it("labels a subagent's edge with its spawning call, and leaves an unlinked one unlabelled", () => {
    expect(edges.find((e) => e.target === "a1")).toMatchObject({ source: "s/main", label: "Find callers", animated: true });
    expect(edges.find((e) => e.target === "a2")).toMatchObject({ source: "s/main", label: undefined, animated: false });
  });

  it("puts subagents below the main agent and utility calls in a pill column to its left", () => {
    const at = Object.fromEntries(nodes.map((n) => [n.id, n]));
    expect(at["a1"]!.y).toBeGreaterThan(at["s/main"]!.y);
    expect(at["a1"]!.x).not.toBe(at["a2"]!.x);
    expect(at["s/background_title"]).toMatchObject({ pill: true });
    expect(at["s/background_title"]!.x).toBeLessThan(at["s/main"]!.x);
    expect(at["a1"]!.toolCount).toBe(1);
  });

  it("hangs a subagent whose parent is unknown under the first main agent", () => {
    const g = buildGraph([agent("s/main"), agent("orphan", { role: "subagent", parent_agent_id: "gone" })], [], []);
    expect(g.edges).toEqual([expect.objectContaining({ source: "s/main", target: "orphan" })]);
  });
});
