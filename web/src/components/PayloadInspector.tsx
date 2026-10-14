import { X } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { fmtMs, fmtTokens } from "../lib/format";
import type { AgentNode, InferenceSpan, ToolInvocation } from "../types/telemetry";

const tabs = ["IDE context", "Tools", "Output & prompts", "Raw"] as const;
type Tab = (typeof tabs)[number];

interface Props {
  agent?: AgentNode;
  span?: InferenceSpan;
  tools: ToolInvocation[]; // Live tool state for the span
  onClose: () => void;
}

export function PayloadInspector({ agent, span, tools, onClose }: Props) {
  const [tab, setTab] = useState<Tab>("IDE context");

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const blocks = span?.injected_ide_blocks ?? [];
  const counts: Partial<Record<Tab, number>> = { "IDE context": blocks.length, Tools: tools.length };

  return (
    <aside className="flex w-[460px] shrink-0 flex-col border-l border-line bg-panel" aria-label="Payload inspector">
      <div className="flex items-start gap-2 border-b border-line px-4 py-3">
        <div className="min-w-0 flex-1">
          <div className="text-sm font-semibold">
            {agent ? (agent.role === "main" ? "Main Agent" : agent.subagent_type ?? agent.role) : "Span"}
          </div>
          <div className="truncate font-mono text-xs text-gray-400" title={span?.id}>
            {span ? `${span.id} · ${span.model}` : "No inference recorded for this agent yet"}
          </div>
          {span && (
            <div className="mt-1 font-mono text-xs text-gray-500">
              {span.status}
              {span.stop_reason ? ` (${span.stop_reason})` : ""} · TTFT {fmtMs(span.ttft_ms)} · {fmtMs(span.duration_ms)} ·
              {" "}hit {(span.cache_hit_ratio * 100).toFixed(0)}%
            </div>
          )}
        </div>
        <button onClick={onClose} className="rounded p-1 text-gray-400 hover:bg-white/10" aria-label="Close inspector">
          <X className="size-4" />
        </button>
      </div>

      <div role="tablist" className="flex border-b border-line text-xs">
        {tabs.map((t) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            onClick={() => setTab(t)}
            className={`flex-1 px-2 py-2 ${tab === t ? "border-b-2 border-vscode text-gray-100" : "text-gray-400 hover:text-gray-200"}`}
          >
            {t}
            {counts[t] ? <span className="ml-1 text-gray-500">{counts[t]}</span> : null}
          </button>
        ))}
      </div>

      <div className="flex-1 overflow-auto p-4 text-xs">
        {!span ? (
          <Empty>Select a span in the waterfall, or an agent that has sent a request.</Empty>
        ) : tab === "IDE context" ? (
          <IdeContext span={span} />
        ) : tab === "Tools" ? (
          <Tools tools={tools} />
        ) : tab === "Output & prompts" ? (
          <div className="flex flex-col gap-4">
            <Text title="Last user message" text={span.last_user_message_preview} />
            <Text title="Assistant output" text={span.assistant_output_preview} live={span.status === "decoding"} />
            <Text title="System prompt (preview)" text={span.system_prompt_preview} />
          </div>
        ) : (
          <Raw span={span} />
        )}
      </div>
    </aside>
  );
}

function IdeContext({ span }: { span: InferenceSpan }) {
  const blocks = span.injected_ide_blocks ?? [];
  if (blocks.length === 0) return <Empty>No &lt;ide_*&gt; blocks in this request.</Empty>;
  const totalTokens = blocks.reduce((n, b) => n + b.est_tokens, 0);
  return (
    <div className="flex flex-col gap-3">
      <div className="text-gray-400">
        {blocks.length} block{blocks.length === 1 ? "" : "s"}, about {fmtTokens(totalTokens)} tokens of context
      </div>
      {[...blocks].sort((a, b) => b.est_tokens - a.est_tokens).map((b, i) => (
        <div key={i} className="rounded border border-line bg-canvas p-2.5">
          <div className="mb-1 flex justify-between gap-2">
            <span className="text-vscode">{b.tag_type}</span>
            <span className="font-mono text-gray-400">~{fmtTokens(b.est_tokens)} tok</span>
          </div>
          {b.file_path && <div className="mb-1 break-all font-mono text-gray-300">{b.file_path}</div>}
          <pre className="whitespace-pre-wrap break-words text-gray-500">{b.preview}</pre>
        </div>
      ))}
    </div>
  );
}

const toolStatus: Record<string, string> = {
  streaming_args: "text-gray-400", running: "text-amber-300", completed: "text-cli", failed: "text-red-400",
};

function Tools({ tools }: { tools: ToolInvocation[] }) {
  if (tools.length === 0) return <Empty>This response called no tools.</Empty>;
  return (
    <ul className="flex flex-col gap-2">
      {tools.map((t) => (
        <li key={t.tool_use_id} className="rounded border border-line bg-canvas p-2.5">
          <div className="flex justify-between gap-2">
            <span className="font-semibold">{t.tool_name}</span>
            <span className={toolStatus[t.status]}>
              {t.status.replace("_", " ")}
              {t.duration_ms ? ` · ${fmtMs(t.duration_ms)}` : ""}
            </span>
          </div>
          {t.target && <div className="mt-1 break-all font-mono text-gray-400">{t.target}</div>}
        </li>
      ))}
    </ul>
  );
}

function Text({ title, text, live }: { title: string; text: string; live?: boolean }) {
  return (
    <section>
      <h3 className="mb-1 text-gray-400">
        {title}
        {live && <span className="ml-2 text-cli">● live</span>}
      </h3>
      {text ? (
        <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded border border-line bg-canvas p-2.5 text-gray-200">{text}</pre>
      ) : (
        <Empty>Empty.</Empty>
      )}
    </section>
  );
}

interface RawPayload {
  request: string;
  response: string;
  request_too_large: boolean;
  response_too_large: boolean;
}

// Raw shows the span record and, on request, its raw bytes as plain text.
// The bytes are never parsed here: the UI consumes only the canonical schema.
function Raw({ span }: { span: InferenceSpan }) {
  const [raw, setRaw] = useState<RawPayload | "loading" | "gone" | null>(null);
  useEffect(() => setRaw(null), [span.id]);
  const load = async () => {
    setRaw("loading");
    const resp = await fetch(`/api/spans/${encodeURIComponent(span.id)}/raw`);
    setRaw(resp.ok ? ((await resp.json()) as RawPayload) : "gone");
  };
  return (
    <div className="flex flex-col gap-4">
      <section>
        <h3 className="mb-1 text-gray-400">InferenceSpan</h3>
        <pre className="max-h-80 overflow-auto rounded border border-line bg-canvas p-2.5 text-gray-300">{JSON.stringify(span, null, 2)}</pre>
      </section>
      <section>
        <h3 className="mb-1 text-gray-400">Raw payload (memory only)</h3>
        {raw === null && (
          <button onClick={load} className="rounded border border-line px-3 py-1.5 hover:bg-white/5">Load raw request and response</button>
        )}
        {raw === "loading" && <Empty>Loading…</Empty>}
        {raw === "gone" && <Empty>No longer in memory: raw payloads are kept for recent spans only, and never after a restart.</Empty>}
        {typeof raw === "object" && raw !== null && (
          <div className="flex flex-col gap-3">
            <RawText title="Request" text={raw.request} tooLarge={raw.request_too_large} />
            <RawText title="Response" text={raw.response} tooLarge={raw.response_too_large} />
          </div>
        )}
      </section>
    </div>
  );
}

function RawText({ title, text, tooLarge }: { title: string; text: string; tooLarge: boolean }) {
  return (
    <div>
      <div className="mb-1 text-gray-500">{title} · {tooLarge ? "too large to keep" : `${text.length.toLocaleString()} chars`}</div>
      {!tooLarge && <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-all rounded border border-line bg-canvas p-2.5 text-gray-300">{text}</pre>}
    </div>
  );
}

function Empty({ children }: { children: ReactNode }) {
  return <div className="text-gray-500">{children}</div>;
}
