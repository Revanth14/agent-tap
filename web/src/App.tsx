import { useMemo, useState } from "react";
import { TopHudBar } from "./components/TopHudBar";
import { useTelemetrySocket } from "./hooks/useTelemetrySocket";
import { sessionView, sessionsByRecency } from "./lib/state";

export default function App() {
  const { state, connection } = useTelemetrySocket();
  const sessions = useMemo(() => sessionsByRecency(state), [state]);
  const [picked, setPicked] = useState<string | null>(null);
  // Follow the most recently active session until the user picks one.
  const selectedId = picked && state.sessions[picked] ? picked : sessions[0]?.id;
  const view = useMemo(() => (selectedId ? sessionView(state, selectedId) : null), [state, selectedId]);

  return (
    <div className="flex h-full flex-col">
      <TopHudBar sessions={sessions} view={view} onSelect={setPicked} connection={connection} planUsage={state.planUsage} />
      <main className="grid flex-1 place-items-center p-4 text-sm text-gray-500">
        {view ? `${view.agents.length} agents · ${view.spans.length} spans · ${view.tools.length} tools` : "Waiting for Claude Code traffic…"}
      </main>
    </div>
  );
}
