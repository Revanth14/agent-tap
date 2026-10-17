import { useEffect, useState } from "react";
import type { PlanView } from "../types/telemetry";

const REFRESH_MS = 30_000; // Keeps the burn rate and time to limit current while idle

// usePlan fetches GET /api/plan, again whenever trigger changes (pass the
// live plan usage, which every response carrying usage headers replaces) and
// every 30 seconds.
export function usePlan(trigger: unknown): PlanView | null {
  const [plan, setPlan] = useState<PlanView | null>(null);
  useEffect(() => {
    let live = true;
    const load = () =>
      fetch("/api/plan")
        .then((r) => (r.ok ? (r.json() as Promise<PlanView>) : null))
        .then((p) => live && p && setPlan(p))
        .catch(() => {});
    load();
    const timer = setInterval(load, REFRESH_MS);
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [trigger]);
  return plan;
}
