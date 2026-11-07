import type { Transport } from "@connectrpc/connect";
import { useEffect, useState, useSyncExternalStore } from "react";
import { WatchEngine, type WatchState } from "./watch";

export interface WatchHandle extends WatchState {
  setToken: (token: string) => void;
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

const noop = () => {};
const noSubscribe = () => noop;

export function useWatchEvents(transport: Transport, sessionId: string, getToken: () => string): WatchHandle {
  const [engine, setEngine] = useState<WatchEngine | undefined>();

  useEffect(() => {
    if (!sessionId) {
      setEngine(undefined);
      return;
    }
    const e = new WatchEngine({ transport, sessionId, getToken });
    setEngine(e);
    e.start();
    return () => e.stop();
    // getToken is read on each connect, so a new identity needn't restart the watch.
  }, [transport, sessionId]);

  const state = useSyncExternalStore(engine?.subscribe ?? noSubscribe, engine?.getSnapshot ?? (() => idle));
  return {
    ...state,
    setToken: (t) => engine?.setToken(t),
    resumeAuth: () => engine?.resumeAuth(),
  };
}
