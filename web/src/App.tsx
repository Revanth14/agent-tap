import { useCallback, useMemo, useState } from "react";
import { AgentDagView } from "./components/AgentDagView";
import { PayloadInspector } from "./components/PayloadInspector";
import { TokenWaterfall } from "./components/TokenWaterfall";
import { TopHudBar } from "./components/TopHudBar";
import { useTelemetrySocket } from "./hooks/useTelemetrySocket";
import { sessionView, sessionsByRecency } from "./lib/state";

type Selection = { kind: "agent" | "span"; id: string } | null;

export default function App() {
  const { state, connection } = useTelemetrySocket();
  const sessions = useMemo(() => sessionsByRecency(state), [state]);
  const [picked, setPicked] = useState<string | null>(null);
  // Follow the most recently active session until the user picks one.
  const selectedId = picked && state.sessions[picked] ? picked : sessions[0]?.id;
  const view = useMemo(() => (selectedId ? sessionView(state, selectedId) : null), [state, selectedId]);
  const [selection, setSelection] = useState<Selection>(null);
  const close = useCallback(() => setSelection(null), []);

  // An agent selection inspects that agent's latest span.
  const span =
    selection?.kind === "span"
      ? view?.spans.find((s) => s.id === selection.id)
      : selection?.kind === "agent"
        ? view?.spans.findLast((s) => s.agent_id === selection.id)
        : undefined;
  const agentId = selection?.kind === "agent" ? selection.id : span?.agent_id;
  const agent = view?.agents.find((a) => a.id === agentId);
  const spanTools = useMemo(() => view?.tools.filter((t) => t.span_id === span?.id) ?? [], [view, span?.id]);

  return (
    <div className="flex h-full flex-col">
      <TopHudBar
        sessions={sessions}
        view={view}
        onSelect={(id) => {
          setPicked(id);
          setSelection(null);
        }}
        connection={connection}
        planUsage={state.planUsage}
      />
      {view ? (
        <div className="flex min-h-0 flex-1">
          <main className="flex min-w-0 flex-1 flex-col">
            <section className="h-[45%] min-h-56 border-b border-line" aria-label="Agent graph">
              <AgentDagView
                view={view}
                selectedAgent={selection?.kind === "agent" ? selection.id : null}
                onSelectAgent={(id) => setSelection({ kind: "agent", id })}
              />
            </section>
            <section className="min-h-0 flex-1" aria-label="Inference waterfall">
              <TokenWaterfall
                view={view}
                selectedAgent={selection?.kind === "agent" ? selection.id : null}
                selectedSpan={span?.id ?? null}
                onSelectSpan={(id) => setSelection({ kind: "span", id })}
              />
            </section>
          </main>
          {selection && <PayloadInspector agent={agent} span={span} tools={spanTools} onClose={close} />}
        </div>
      ) : (
        <main className="grid flex-1 place-items-center text-sm text-gray-500">
          {connection === "live" ? "Waiting for Claude Code traffic…" : "Connecting to the agent-tap daemon…"}
        </main>
      )}
    </div>
  );
}
