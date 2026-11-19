import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { NetworkEvent } from "../../gen/durable/v1/durable_pb";

export type Verdict = "all" | "denied" | "allowed";

export interface Filter {
  verdict: Verdict;
  text: string;
  // Inclusive millisecond window of the selected call.
  window?: { start: number; end: number };
}

export const rowCap = 500;
export const topLimit = 8;
export const chipHostLimit = 5;

export const eventMs = (e: NetworkEvent): number => (e.time ? timestampDate(e.time).getTime() : 0);

export function matches(e: NetworkEvent, f: Filter): boolean {
  if (f.verdict === "denied" && e.allowed) return false;
  if (f.verdict === "allowed" && !e.allowed) return false;
  if (f.window) {
    const t = eventMs(e);
    if (t < f.window.start || t > f.window.end) return false;
  }
  const q = f.text.trim().toLowerCase();
  return !q || `${e.host}${e.path}`.toLowerCase().includes(q);
}

export interface TopDenied {
  host: string;
  what: string;
  count: number;
}

// A DNS refusal names the host without a port and the connection after it with one, so both group under the host name.
export const hostName = (host: string): string => host.match(/^(\[[^\]]*\]|[^:]*):\d+$/)?.[1] ?? host;

// Grouped by host name and HTTP path pattern, so repeated requests collapse into one row.
export function topDenied(events: NetworkEvent[], limit = topLimit): TopDenied[] {
  const groups = new Map<string, TopDenied>();
  for (const e of events) {
    if (e.allowed) continue;
    const host = hostName(e.host);
    const what = e.pattern;
    const key = `${host}\0${what}`;
    const g = groups.get(key);
    if (g) g.count++;
    else groups.set(key, { host, what, count: 1 });
  }
  return [...groups.values()].sort((a, b) => b.count - a.count || a.host.localeCompare(b.host) || a.what.localeCompare(b.what)).slice(0, limit);
}

const sameIds = (a: NetworkEvent[], b: NetworkEvent[]) => a.length === b.length && a.every((e, i) => e.id === b[i]!.id);

// Reuses the previous array when a call's events are unchanged, so memoised calls skip unrelated polls.
export function stableDenied(prev: Map<string, NetworkEvent[]>, next: Map<string, NetworkEvent[]>): Map<string, NetworkEvent[]> {
  let same = prev.size === next.size;
  const out = new Map<string, NetworkEvent[]>();
  for (const [id, events] of next) {
    const old = prev.get(id);
    if (old && sameIds(old, events)) out.set(id, old);
    else {
      out.set(id, events);
      same = false;
    }
  }
  return same ? prev : out;
}

export function hostSummary(events: NetworkEvent[], limit = chipHostLimit): string {
  const hosts = [...new Set(events.map((e) => hostName(e.host) || "unknown host"))];
  const shown = hosts.slice(0, limit).join(", ");
  return hosts.length > limit ? `${shown} +${hosts.length - limit} more` : shown;
}
