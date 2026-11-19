import { useMemo, useRef } from "react";
import type { NetworkEvent } from "../../gen/durable/v1/durable_pb";
import { useNow } from "../../ui/useNow";
import { isRunning } from "../transcript/calls";
import type { CallEntry, Turn } from "../transcript/transcript";
import { deniedByCall, type NetworkState } from "./network";
import { stableDenied } from "./networkView";

export function useDenied(net: NetworkState | undefined, turns: Turn[]): { allCalls: CallEntry[]; deniedCount: number; denied: Map<string, NetworkEvent[]> } {
  const allCalls = useMemo(() => turns.flatMap((t) => t.entries.filter((e): e is CallEntry => e.kind === "call")), [turns]);
  const deniedCount = useMemo(() => net?.events.filter((e) => !e.allowed).length ?? 0, [net?.events]);
  // A running call's window grows, so the clock ticks only while a call runs and something was denied.
  const ticking = deniedCount > 0 && allCalls.some((c) => c.startedAt !== undefined && isRunning(c));
  const netNow = useNow(ticking);
  const prevDenied = useRef<Map<string, NetworkEvent[]>>(new Map());
  const denied = useMemo(() => {
    prevDenied.current = stableDenied(prevDenied.current, deniedByCall(net?.events ?? [], allCalls, netNow));
    return prevDenied.current;
  }, [net?.events, allCalls, netNow]);
  return { allCalls, deniedCount, denied };
}
