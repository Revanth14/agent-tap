import { Activity, ArrowLeft, Code2, Gauge, Layers, Terminal, Users } from "lucide-react";
import type { ReactNode } from "react";
import type { Connection } from "../hooks/useTelemetrySocket";
import { fmtPct, fmtReset, fmtTokens } from "../lib/format";
import type { SessionView } from "../lib/state";
import type { PlanUsage, Session, SurfaceType } from "../types/telemetry";

const COMPACT_WARN = 0.75; // Warn at 75% of the window, so it scales between 200K and 1M.

interface Props {
  onBack?: () => void; // Absent while there is no plan usage to go back to
  sessions: Session[];
  view: SessionView | null;
  onSelect: (id: string) => void;
  connection: Connection;
  planUsage: PlanUsage | null;
}

export function TopHudBar({ onBack, sessions, view, onSelect, connection, planUsage }: Props) {
  const s = view?.session;
  const input = s ? s.total_input_tokens + s.total_cache_write + s.total_cache_read : 0;
  const lastSpan = view?.spans.at(-1);
  const activeSurface: SurfaceType | null =
    lastSpan && (lastSpan.status === "prefill" || lastSpan.status === "decoding") ? lastSpan.surface : null;
  const mains = view?.agents.filter((a) => a.role === "main").length ?? 0;
  const subs = view?.agents.filter((a) => a.role === "subagent" && a.status !== "completed" && a.status !== "error").length ?? 0;
  const fill = s && s.max_context_window > 0 ? s.current_context_size / s.max_context_window : 0;

  return (
    <header className="flex flex-wrap items-stretch gap-3 border-b border-line bg-panel px-4 py-3">
      <div className="flex min-w-64 flex-col justify-center gap-1.5">
        <div className="flex items-center gap-2">
          {onBack && (
            <button type="button" onClick={onBack} className="flex items-center gap-1 rounded px-1 text-xs text-gray-400 hover:bg-canvas hover:text-gray-200">
              <ArrowLeft className="size-3.5" aria-hidden />
              Plan
            </button>
          )}
          <Activity className="size-4 text-cli" aria-hidden />
          <span className="font-semibold tracking-tight">agent-tap</span>
          <ConnectionPill connection={connection} />
          {s?.id.startsWith("demo-") && (
            <span className="rounded-full border border-amber-500/40 px-2 py-0.5 text-xs text-amber-300">Demo traffic</span>
          )}
        </div>
        <select
          aria-label="Session"
          className="max-w-80 truncate rounded border border-line bg-canvas px-2 py-1 font-mono text-xs"
          value={s?.id ?? ""}
          onChange={(e) => onSelect(e.target.value)}
        >
          {sessions.length === 0 && <option value="">No sessions yet</option>}
          {sessions.map((x) => (
            <option key={x.id} value={x.id}>
              {surfaceName(x.primary_surface)} · {basename(x.workspace_path) || "?"} · {x.id.slice(0, 8)}
            </option>
          ))}
        </select>
      </div>

      <div className="flex items-center gap-2">
        <SurfaceBadge kind="vscode_ext" primary={s?.primary_surface} active={activeSurface} />
        <SurfaceBadge kind="terminal_cli" primary={s?.primary_surface} active={activeSurface} />
      </div>

      <Card icon={<Gauge className="size-3.5" aria-hidden />} title="Plan usage">
        {planUsage ? (
          <div className="flex gap-3">
            {planUsage.windows.map((w) => (
              <span key={w.name} className={w.utilization >= 0.9 ? "text-red-400" : w.utilization >= 0.7 ? "text-amber-300" : ""}>
                {w.name} {fmtPct(w.utilization, 0)}
                {w.resets_at && <span className="text-gray-500"> · {fmtReset(w.resets_at)}</span>}
              </span>
            ))}
          </div>
        ) : (
          <span className="text-gray-500">—</span>
        )}
      </Card>

      <Card icon={<Layers className="size-3.5" aria-hidden />} title="Cache hit ratio">
        {input > 0 ? (
          <>
            {fmtPct(s!.total_cache_read / input)}{" "}
            <span className="text-gray-500">({fmtTokens(s!.total_cache_read)} from cache)</span>
          </>
        ) : (
          <span className="text-gray-500">—</span>
        )}
      </Card>

      <Card icon={<Users className="size-3.5" aria-hidden />} title="Active agents">
        {mains} main, {subs} subagent{subs === 1 ? "" : "s"}
      </Card>

      <div className="flex min-w-56 flex-1 flex-col justify-center gap-1">
        <div className="flex justify-between text-xs">
          <span className="text-gray-400">Context</span>
          <span className="font-mono">
            {s ? `${fmtTokens(s.current_context_size)} / ${fmtTokens(s.max_context_window)}` : "—"}
          </span>
        </div>
        <div className="h-2 overflow-hidden rounded bg-canvas" role="meter" aria-label="Context window fill" aria-valuenow={Math.round(fill * 100)} aria-valuemin={0} aria-valuemax={100}>
          <div className={`h-full ${fill >= COMPACT_WARN ? "bg-amber-500" : "bg-cli"}`} style={{ width: `${Math.min(fill, 1) * 100}%` }} />
        </div>
        <div className="h-4 text-xs">
          {fill >= COMPACT_WARN ? (
            <span className="text-amber-300">Auto-compact approaching</span>
          ) : (
            <span className="text-gray-600">Fast-mode calls are not visible to agent-tap</span>
          )}
        </div>
      </div>
    </header>
  );
}

function Card({ icon, title, children }: { icon: ReactNode; title: string; children: ReactNode }) {
  return (
    <div className="flex min-w-40 flex-col justify-center rounded border border-line bg-canvas px-3 py-1.5">
      <div className="flex items-center gap-1.5 text-xs text-gray-400">
        {icon}
        {title}
      </div>
      <div className="font-mono text-sm">{children}</div>
    </div>
  );
}

export function ConnectionPill({ connection }: { connection: Connection }) {
  const style = {
    live: "border-cli/40 text-cli",
    connecting: "border-gray-600 text-gray-400",
    offline: "border-red-500/50 text-red-400",
  }[connection];
  const text = { live: "Live", connecting: "Connecting", offline: "Daemon offline" }[connection];
  return <span className={`rounded-full border px-2 py-0.5 text-xs ${style}`}>{text}</span>;
}

function SurfaceBadge({ kind, primary, active }: { kind: SurfaceType; primary?: SurfaceType; active: SurfaceType | null }) {
  const vscode = kind === "vscode_ext";
  const color = vscode ? "var(--color-vscode)" : "var(--color-cli)";
  const isActive = active === kind;
  const Icon = vscode ? Code2 : Terminal;
  return (
    <div
      className={`flex items-center gap-1.5 rounded border px-2.5 py-1.5 text-xs transition-shadow ${primary === kind ? "" : "opacity-40"}`}
      style={{ borderColor: color, color, boxShadow: isActive ? `0 0 12px ${color}` : undefined }}
      title={isActive ? "Streaming now" : undefined}
    >
      <Icon className="size-3.5" aria-hidden />
      {vscode ? "VS Code Extension" : "Terminal CLI"}
    </div>
  );
}

export function surfaceName(s: SurfaceType): string {
  return s === "vscode_ext" ? "VS Code" : s === "terminal_cli" ? "CLI" : "?";
}

export function basename(path: string): string {
  return path.split("/").filter(Boolean).at(-1) ?? "";
}
