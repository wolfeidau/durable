import type { Transport } from "@connectrpc/connect";
import { NetworkPoller, type NetworkState } from "./network";
import { useEngine } from "../../ui/useEngine";

const none = () => undefined;

export function useNetwork(transport: Transport, sessionId: string, since: Date | undefined, enabled: boolean): NetworkState | undefined {
  const sinceMs = since?.getTime();
  const { state } = useEngine<NetworkState | undefined, NetworkPoller>(
    () => (!enabled || sinceMs === undefined || !sessionId ? undefined : new NetworkPoller({ transport, sessionId, since: new Date(sinceMs) })),
    [transport, sessionId, sinceMs, enabled],
    none,
  );
  return state;
}
