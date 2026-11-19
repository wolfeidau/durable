import type { Transport } from "@connectrpc/connect";
import { WatchEngine, type WatchState } from "./watch";
import { useEngine } from "../ui/useEngine";

export interface WatchHandle extends WatchState {
  resumeAuth: () => void;
}

const idle: WatchState = {
  status: "stopped",
  epoch: "",
  cursor: 0n,
  events: [],
  gaps: 0,
  reconnects: 0,
  lastHeartbeat: undefined,
  partialText: new Map(),
  toolOutput: new Map(),
};

const getIdle = () => idle;

export function useWatchEvents(transport: Transport, sessionId: string, getToken: () => string): WatchHandle {
  // getToken is read on each connect, so a new identity needn't restart the watch.
  const { engine, state } = useEngine(() => (sessionId ? new WatchEngine({ transport, sessionId, getToken }) : undefined), [transport, sessionId], getIdle);
  return {
    ...state,
    resumeAuth: () => engine?.resumeAuth(),
  };
}
