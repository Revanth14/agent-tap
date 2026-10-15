import { useEffect, useRef } from "react";
import { fmtClock, fmtMs, fmtTokens } from "../lib/format";
import type { SessionView } from "../lib/state";
import type { InferenceSpan } from "../types/telemetry";

const roleName: Record<string, string> = {
  main: "Main", subagent: "Subagent", background_title: "Title", compactor: "Compactor", fast_utility: "Helper",
};

// The bar shows the tokens that change from turn to turn. Cache reads get
// their own column: in a long session every turn re-reads hundreds of
// thousands of cached tokens, which would fill every bar identically and hide
// the output and new input that differ.
const tiers = [
  { key: "input_tokens_cache_write", label: "Cache write", color: "var(--color-cache-write)" },
  { key: "input_tokens_uncached", label: "Uncached input", color: "var(--color-uncached)" },
  { key: "output_tokens", label: "Output", color: "var(--color-output)" },
] as const;

const fresh = (sp: InferenceSpan) => tiers.reduce((n, t) => n + sp[t.key], 0);
const total = (sp: InferenceSpan) => fresh(sp) + sp.input_tokens_cache_read;

interface Props {
  view: SessionView;
  selectedAgent: string | null;
  selectedSpan: string | null;
  onSelectSpan: (id: string) => void;
}

export function TokenWaterfall({ view, selectedAgent, selectedSpan, onSelectSpan }: Props) {
  const max = Math.max(1, ...view.spans.map(fresh));
  const toolsBySpan = new Map<string, number>();
  for (const t of view.tools) toolsBySpan.set(t.span_id, (toolsBySpan.get(t.span_id) ?? 0) + 1);

  // Stay pinned to the newest row unless the user has scrolled up.
  const scroller = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);
  useEffect(() => {
    const el = scroller.current;
    if (el && pinned.current) el.scrollTop = el.scrollHeight;
  }, [view.spans.length]);

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-4 border-b border-line px-3 py-2 text-xs text-gray-400">
        <span className="font-semibold text-gray-200">Inference waterfall</span>
        {tiers.map((t) => (
          <span key={t.key} className="flex items-center gap-1.5">
            <span className="size-2.5 rounded-sm" style={{ background: t.color }} />
            {t.label}
          </span>
        ))}
        <span className="flex items-center gap-1.5">
          <span className="size-2.5 rounded-sm" style={{ background: "var(--color-cache-read)" }} />
          Cache read (own column)
        </span>
      </div>
      <div
        ref={scroller}
        className="flex-1 overflow-auto"
        onScroll={(e) => {
          const el = e.currentTarget;
          pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
        }}
      >
        <table className="w-full border-collapse font-mono text-xs">
          <thead className="sticky top-0 z-10 bg-panel text-left text-gray-400">
            <tr>
              {["Time", "Surface", "Agent role", "Model", "TTFT", "Speed", "New tokens", "Cached", "Tools", "Total"].map((h) => (
                <th key={h} className="whitespace-nowrap px-3 py-1.5 font-normal" title={h === "Speed" ? "Output tokens ÷ whole request time" : undefined}>
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {view.spans.map((sp) => {
              const dim = selectedAgent && sp.agent_id !== selectedAgent ? "opacity-35" : "";
              const sel = sp.id === selectedSpan ? "bg-vscode/15" : "hover:bg-white/5";
              const n = fresh(sp);
              const hit = total(sp) > 0 ? sp.input_tokens_cache_read / total(sp) : 0;
              return (
                <tr key={sp.id} className={`cursor-pointer border-b border-line/60 ${dim} ${sel}`} onClick={() => onSelectSpan(sp.id)}>
                  <td className="whitespace-nowrap px-3 py-1.5 text-gray-400">{fmtClock(sp.started_at)}</td>
                  <td className={`px-3 py-1.5 ${sp.surface === "vscode_ext" ? "text-vscode" : "text-cli"}`}>
                    {sp.surface === "vscode_ext" ? "VS Code" : sp.surface === "terminal_cli" ? "CLI" : "?"}
                  </td>
                  <td className="whitespace-nowrap px-3 py-1.5">
                    {roleName[sp.role] ?? sp.role}
                    {sp.status === "error" && (
                      <span className="ml-2 rounded bg-red-500/20 px-1.5 text-red-300">{sp.stop_reason || "error"}</span>
                    )}
                    {(sp.status === "prefill" || sp.status === "decoding") && <span className="ml-2 text-cli">●</span>}
                  </td>
                  <td className="whitespace-nowrap px-3 py-1.5 text-gray-400">{sp.model.replace(/^claude-/, "")}</td>
                  <td className="px-3 py-1.5 text-right">{sp.first_token_at ? fmtMs(sp.ttft_ms) : "—"}</td>
                  <td className="px-3 py-1.5 text-right">{sp.tokens_per_sec > 0 ? `${Math.round(sp.tokens_per_sec)}/s` : "—"}</td>
                  <td className="w-full min-w-48 px-3 py-1.5">
                    <div className="flex items-center gap-2">
                      <div
                        className="flex h-3 overflow-hidden rounded-sm"
                        style={{ width: `${(n / max) * 100}%`, minWidth: n > 0 ? 2 : 0 }}
                        title={[...tiers.map((t) => `${t.label}: ${sp[t.key].toLocaleString()}`), `Cache read: ${sp.input_tokens_cache_read.toLocaleString()}`].join("\n")}
                      >
                        {tiers.map((t) => (
                          <div key={t.key} style={{ width: `${(sp[t.key] / Math.max(n, 1)) * 100}%`, background: t.color }} />
                        ))}
                      </div>
                      {sp.cache_invalidated && (
                        <span className="shrink-0 rounded bg-red-600 px-1.5 py-0.5 text-[10px] font-bold text-white">CACHE INVALIDATED</span>
                      )}
                    </div>
                  </td>
                  <td className="whitespace-nowrap px-3 py-1.5 text-right" style={{ color: "var(--color-cache-read)" }}>
                    {sp.input_tokens_cache_read > 0 ? `${fmtTokens(sp.input_tokens_cache_read)} · ${Math.round(hit * 100)}%` : "—"}
                  </td>
                  <td className="px-3 py-1.5 text-right">{toolsBySpan.get(sp.id) ?? ""}</td>
                  <td className="px-3 py-1.5 text-right">{fmtTokens(total(sp))}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}
