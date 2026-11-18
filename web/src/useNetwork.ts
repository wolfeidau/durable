import type { Transport } from "@connectrpc/connect";
import { useEffect, useState, useSyncExternalStore } from "react";
import { NetworkPoller, type NetworkState } from "./network";

const noop = () => {};
const noSubscribe = () => noop;
const none = () => undefined;

export function useNetwork(transport: Transport, sessionId: string, since: Date | undefined, enabled: boolean): NetworkState | undefined {
  const [poller, setPoller] = useState<NetworkPoller | undefined>();
  const sinceMs = since?.getTime();

  useEffect(() => {
    if (!enabled || sinceMs === undefined || !sessionId) {
      setPoller(undefined);
      return;
    }
    const p = new NetworkPoller({ transport, sessionId, since: new Date(sinceMs) });
    setPoller(p);
    p.start();
    return () => p.stop();
  }, [transport, sessionId, sinceMs, enabled]);

  return useSyncExternalStore(poller?.subscribe ?? noSubscribe, poller?.getSnapshot ?? none);
}
