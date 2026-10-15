import {
  Background, Controls, Handle, Position, ReactFlow, ReactFlowProvider, useNodesInitialized, useReactFlow,
  type Edge, type Node, type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { Bot, Code2, Sparkles, Terminal } from "lucide-react";
import { memo, useEffect, useMemo } from "react";
import { buildGraph, type GraphNode } from "../lib/dag";
import { fmtMs, fmtTokens } from "../lib/format";
import type { SessionView } from "../lib/state";

type AgentFlowNode = Node<{ g: GraphNode; selected: boolean }, "agent">;

interface Props {
  view: SessionView;
  selectedAgent: string | null;
  onSelectAgent: (id: string) => void;
}

export function AgentDagView(props: Props) {
  // A fresh flow per session, so each session starts fitted.
  return (
    <ReactFlowProvider key={props.view.session.id}>
      <Graph {...props} />
    </ReactFlowProvider>
  );
}

const nodeTypes = { agent: memo(AgentCard) };
const fitOptions = { padding: 0.2, maxZoom: 1.1 }; // Never blow a lone node up past readable size

function Graph({ view, selectedAgent, onSelectAgent }: Props) {
  const { nodes, edges } = useMemo(() => {
    const g = buildGraph(view.agents, view.spans, view.tools);
    return {
      nodes: g.nodes.map<AgentFlowNode>((n) => ({
        id: n.id, type: "agent", position: { x: n.x, y: n.y },
        data: { g: n, selected: n.id === selectedAgent }, draggable: false,
      })),
      edges: g.edges.map<Edge>((e) => ({
        id: e.id, source: e.source, target: e.target, label: e.label, animated: e.animated,
        style: { stroke: "#4b5563" }, labelStyle: { fill: "#d1d5db", fontSize: 11 },
        labelBgStyle: { fill: "#111827" }, labelBgPadding: [6, 3],
      })),
    };
  }, [view, selectedAgent]);

  // Refit when agents appear, not on every update, so live streaming does
  // not fight the user's pan and zoom. Fitting before React Flow has
  // measured the new nodes would fit nothing, so wait for that.
  const { fitView } = useReactFlow();
  const measured = useNodesInitialized();
  useEffect(() => {
    if (measured) fitView({ ...fitOptions, duration: 250 });
  }, [measured, nodes.length, fitView]);

  return (
    <ReactFlow
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      colorMode="dark"
      fitView
      fitViewOptions={fitOptions}
      minZoom={0.2}
      nodesConnectable={false}
      // Scrolling over the graph must not zoom it: a trackpad scroll meant
      // for the page shrank the nodes out of sight. Pinch still zooms.
      zoomOnScroll={false}
      onNodeClick={(_, n) => onSelectAgent(n.id)}
      style={{ background: "transparent" }}
    >
      <Background color="#1f2937" gap={24} />
      <Controls showInteractive={false} position="bottom-left" />
    </ReactFlow>
  );
}

const statusColor: Record<string, string> = {
  streaming: "bg-cli animate-pulse",
  running_tool: "bg-amber-400 animate-pulse",
  idle: "bg-gray-500",
  completed: "bg-gray-600",
  error: "bg-red-500",
};

function AgentCard({ data }: NodeProps<AgentFlowNode>) {
  const { g, selected } = data;
  const a = g.agent;
  const sp = g.lastSpan;
  const ring = selected ? "ring-2 ring-vscode" : "";

  if (g.pill) {
    return (
      <div className={`flex items-center gap-2 rounded-full border border-line bg-panel px-3 py-1.5 text-xs ${ring}`}>
        <Handle type="target" position={Position.Right} className="!opacity-0" />
        <span className={`size-2 rounded-full ${statusColor[a.status]}`} />
        <Sparkles className="size-3 text-gray-400" aria-hidden />
        {{ background_title: "Background Title Gen", compactor: "Compactor", fast_utility: "Helper call" }[a.role as string] ?? a.role}
        <span className="font-mono text-gray-500">{fmtTokens(a.total_tokens)}</span>
      </div>
    );
  }

  const main = a.role === "main";
  const live =
    a.status === "streaming" && sp?.status === "decoding"
      ? `Streaming${sp.tokens_per_sec > 0 ? ` ${Math.round(sp.tokens_per_sec)} tok/s` : ""}`
      : a.status === "running_tool"
        ? `Running tool${a.active_tool ? `: ${a.active_tool}` : ""}`
        : a.status;
  const duration = a.completed_at ? new Date(a.completed_at).getTime() - new Date(a.started_at).getTime() : null;

  return (
    <div className={`w-60 rounded-lg border bg-panel p-3 text-xs shadow-lg ${main ? "border-gray-500" : "border-line"} ${ring}`}>
      <Handle type="target" position={Position.Top} className="!opacity-0" />
      <Handle type="source" position={Position.Bottom} className="!opacity-0" />
      <Handle type="source" position={Position.Left} className="!opacity-0" />
      <div className="mb-1.5 flex items-center gap-1.5">
        {main ? <Bot className="size-3.5" aria-hidden /> : <Sparkles className="size-3.5 text-vscode" aria-hidden />}
        <span className="font-semibold">{main ? "Main Agent" : (a.subagent_type ?? "Subagent")}</span>
        {main && (
          <span className={`ml-auto flex items-center gap-1 ${a.surface === "vscode_ext" ? "text-vscode" : "text-cli"}`}>
            {a.surface === "vscode_ext" ? <Code2 className="size-3" aria-hidden /> : <Terminal className="size-3" aria-hidden />}
            {a.surface === "vscode_ext" ? "VS Code" : "CLI"}
          </span>
        )}
      </div>
      {a.label && <div className="mb-1.5 line-clamp-2 text-gray-300" title={a.label}>{a.label}</div>}
      <div className="font-mono text-gray-400">{a.model || "—"}</div>
      <div className="mt-1.5 flex items-center gap-1.5">
        <span className={`size-2 shrink-0 rounded-full ${statusColor[a.status]}`} />
        <span className="truncate" title={live}>{live}</span>
      </div>
      <div className="mt-1.5 flex justify-between font-mono text-gray-500">
        <span>{fmtTokens(a.total_tokens)} tok</span>
        <span>{g.toolCount} tools</span>
        {duration !== null && <span>{fmtMs(duration)}</span>}
      </div>
    </div>
  );
}
