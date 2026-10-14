import { useEffect, useReducer, useState } from "react";
import { applyEvent, emptyState } from "../lib/state";
import type { WSEvent } from "../types/telemetry";

export type Connection = "connecting" | "live" | "offline";

// useTelemetrySocket follows the daemon's /api/ws, reconnecting with backoff.
// Each (re)connect starts with a snapshot, which replaces the state.
export function useTelemetrySocket() {
  const [state, dispatch] = useReducer(applyEvent, emptyState);
  const [connection, setConnection] = useState<Connection>("connecting");

  useEffect(() => {
    let ws: WebSocket | null = null;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let delay = 500;
    let closed = false;

    const connect = () => {
      const proto = location.protocol === "https:" ? "wss:" : "ws:";
      ws = new WebSocket(`${proto}//${location.host}/api/ws`);
      ws.onopen = () => {
        delay = 500;
        setConnection("live");
      };
      ws.onmessage = (msg) => dispatch(JSON.parse(msg.data as string) as WSEvent);
      ws.onclose = () => {
        if (closed) return;
        setConnection("offline");
        retry = setTimeout(connect, delay);
        delay = Math.min(delay * 2, 5000);
      };
    };
    connect();
    return () => {
      closed = true;
      clearTimeout(retry);
      ws?.close();
    };
  }, []);

  return { state, connection };
}
