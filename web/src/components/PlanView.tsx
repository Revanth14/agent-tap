import { Activity, AlertTriangle } from "lucide-react";
import type { ReactNode } from "react";
import type { Connection } from "../hooks/useTelemetrySocket";
import { fmtClock, fmtDuration, fmtPct, fmtReset, fmtRise, fmtTokens } from "../lib/format";
import type { PlanSession, PlanView as Plan } from "../types/telemetry";
import { basename, ConnectionPill, surfaceName } from "./TopHudBar";

// The 5h thresholds the status line mod toasts at.
const WARN_AT = 0.8;
const CRITICAL_AT = 0.9;

interface Props {
  plan: Plan | null;
  connection: Connection;
  onOpenSession: (id: string) => void;
}

// PlanView is the landing view: what is eating the current plan window, and
// which session to open to see why.
export function PlanView({ plan, connection, onOpenSession }: Props) {
  return (
    <div className="flex h-full flex-col">
      <header className="flex items-center gap-2 border-b border-line bg-panel px-4 py-3">
        <Activity className="size-4 text-cli" aria-hidden />
        <span className="font-semibold tracking-tight">agent-tap</span>
        <ConnectionPill connection={connection} />
        <span className="ml-2 text-sm text-gray-400">What is eating my plan</span>
      </header>
      {!plan ? (
        <Empty>Loading the plan window…</Empty>
      ) : plan.window === "" ? (
        <Empty>No plan usage yet: it appears with the next Claude Code response.</Empty>
      ) : (
        <main className="min-h-0 flex-1 overflow-auto p-4">
          <Header plan={plan} />
          <SessionTable plan={plan} onOpenSession={onOpenSession} />
        </main>
      )}
    </div>
  );
}

function Header({ plan }: { plan: Plan }) {
  const u = plan.utilization;
  const near = u >= WARN_AT;
  return (
    <section className="mb-4 flex flex-wrap gap-3" aria-label={`${plan.window} plan window`}>
      <Tile title={`${plan.window} plan window`} wide>
        <div className="flex items-baseline gap-3">
          <span className="font-mono text-3xl">{fmtPct(u, 0)}</span>
          {near && (
            <span className={`flex items-center gap-1 text-xs ${u >= CRITICAL_AT ? "text-red-400" : "text-amber-300"}`}>
              <AlertTriangle className="size-3.5" aria-hidden />
              {u >= CRITICAL_AT ? "Past 90%" : "Past 80%"}
            </span>
          )}
        </div>
        <div
          className="relative mt-2 h-2 rounded bg-canvas"
          role="meter"
          aria-label={`${plan.window} utilization`}
          aria-valuenow={Math.round(u * 100)}
          aria-valuemin={0}
          aria-valuemax={100}
        >
          <div className="h-full rounded bg-gray-300" style={{ width: `${Math.min(u, 1) * 100}%` }} />
          {[WARN_AT, CRITICAL_AT].map((t) => (
            <div key={t} className="absolute top-[-2px] h-3 w-px bg-gray-500" style={{ left: `${t * 100}%` }} title={`${t * 100}% alert`} />
          ))}
        </div>
        <div className="mt-1.5 text-xs text-gray-500">
          {plan.starts_at && <>started {fmtReset(plan.starts_at)} · </>}
          {plan.resets_at ? <>resets {fmtReset(plan.resets_at)}</> : "reset time unknown"}
        </div>
      </Tile>
      <Tile title="Burn rate">
        <span className="font-mono text-xl">{plan.rate_per_hour != null ? `+${Math.round(plan.rate_per_hour * 100)}%/h` : "—"}</span>
        <div className="text-xs text-gray-500">{plan.rate_per_hour != null ? "over the last 30 min" : "needs two rises in 30 min"}</div>
      </Tile>
      <Tile title="Time to limit">
        <span className="font-mono text-xl">
          {plan.time_to_limit_seconds != null
            ? `~${fmtDuration(plan.time_to_limit_seconds)}`
            : plan.rate_per_hour != null
              ? "not before reset"
              : "—"}
        </span>
        <div className="text-xs text-gray-500">at the current burn rate</div>
      </Tile>
      {plan.others.map((w) => (
        <Tile key={w.name} title={`${w.name} plan window`}>
          <span className="font-mono text-xl">{fmtPct(w.utilization, 0)}</span>
          {w.resets_at && <div className="text-xs text-gray-500">resets {fmtReset(w.resets_at)}</div>}
        </Tile>
      ))}
    </section>
  );
}

function SessionTable({ plan, onOpenSession }: { plan: Plan; onOpenSession: (id: string) => void }) {
  return (
    <section aria-label="Sessions in this plan window" className="rounded border border-line bg-panel">
      <table className="w-full text-sm">
        <thead className="text-left text-xs text-gray-400">
          <tr className="border-b border-line">
            <th className="px-3 py-2 font-normal">Session</th>
            <th className="px-3 py-2 text-right font-normal" title="Rises while only this session was active">Attributed</th>
            <th className="px-3 py-2 text-right font-normal" title="Rises while several sessions were active; never split">Shared</th>
            <th className="px-3 py-2 text-right font-normal" title="Uncached input, cache write and output in this plan window">New tokens</th>
            <th className="px-3 py-2 text-right font-normal">Cache read</th>
            <th className="px-3 py-2 text-right font-normal">Context</th>
            <th className="px-3 py-2 text-right font-normal">Last active</th>
          </tr>
        </thead>
        <tbody className="font-mono">
          {plan.sessions.length === 0 && (
            <tr>
              <td colSpan={7} className="px-3 py-4 text-center font-sans text-gray-500">
                No Claude Code activity in this plan window yet.
              </td>
            </tr>
          )}
          {plan.sessions.map((r) => (
            <SessionRow key={r.id} row={r} onOpen={() => onOpenSession(r.id)} />
          ))}
          <tr className="border-t border-line text-gray-400">
            <td className="px-3 py-2 font-sans">
              Unseen <span className="text-xs text-gray-500">claude.ai, mobile, other machines, fast mode, or after a break</span>
            </td>
            <td className="px-3 py-2 text-right">{fmtRise(plan.unseen)}</td>
            <td colSpan={5} />
          </tr>
        </tbody>
      </table>
    </section>
  );
}

function SessionRow({ row, onOpen }: { row: PlanSession; onOpen: () => void }) {
  const s = row.session;
  const fresh = row.input_tokens_uncached + row.input_tokens_cache_write + row.output_tokens;
  const surface = s?.primary_surface;
  return (
    <tr
      className="cursor-pointer border-b border-line/60 hover:bg-canvas focus:bg-canvas focus:outline-none"
      tabIndex={0}
      onClick={onOpen}
      onKeyDown={(e) => e.key === "Enter" && onOpen()}
      aria-label={`Open session ${row.id.slice(0, 8)}`}
    >
      <td className="px-3 py-2 font-sans">
        <div className="flex items-center gap-2">
          <span
            className="size-2 shrink-0 rounded-full"
            style={{ background: surface === "vscode_ext" ? "var(--color-vscode)" : surface === "terminal_cli" ? "var(--color-cli)" : "#4b5563" }}
            aria-hidden
          />
          <span>{s ? `${surfaceName(s.primary_surface)} · ${basename(s.workspace_path) || "?"}` : "Older session"}</span>
          <span className="font-mono text-xs text-gray-500">{row.id.slice(0, 8)}</span>
        </div>
      </td>
      <td className="px-3 py-2 text-right">{fmtRise(row.attributed)}</td>
      <td className="px-3 py-2 text-right" title={row.shared_with.length ? `With ${row.shared_with.join(", ")}` : undefined}>
        {fmtRise(row.shared)}
        {row.shared_with.length > 0 && <span className="text-xs text-gray-500"> with {row.shared_with.length}</span>}
      </td>
      <td className="px-3 py-2 text-right">{fmtTokens(fresh)}</td>
      <td className="px-3 py-2 text-right text-gray-400">{fmtTokens(row.input_tokens_cache_read)}</td>
      <td className="px-3 py-2 text-right">{s && s.current_context_size > 0 ? `${fmtTokens(s.current_context_size)} / ${fmtTokens(s.max_context_window)}` : "—"}</td>
      <td className="px-3 py-2 text-right text-gray-400">{row.last_active ? fmtClock(row.last_active) : "—"}</td>
    </tr>
  );
}

function Tile({ title, wide, children }: { title: string; wide?: boolean; children: ReactNode }) {
  return (
    <div className={`flex flex-col justify-center rounded border border-line bg-panel px-4 py-3 ${wide ? "min-w-72 flex-1" : "min-w-44"}`}>
      <div className="mb-1 text-xs text-gray-400">{title}</div>
      {children}
    </div>
  );
}

function Empty({ children }: { children: ReactNode }) {
  return <main className="grid flex-1 place-items-center text-sm text-gray-500">{children}</main>;
}
