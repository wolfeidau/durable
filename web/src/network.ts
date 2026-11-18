import { timestampDate, timestampFromDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient, type Client, type Transport } from "@connectrpc/connect";
import {
  NetworkSource,
  SessionService,
  type ListNetworkEventsResponse,
  type NetworkCoverage,
  type NetworkDrop,
  type NetworkEvent,
} from "./gen/durable/v1/durable_pb";

export type NetworkStatus = "loading" | "live" | "no-sandbox" | "stopped";

export interface NetworkState {
  status: NetworkStatus;
  // Oldest first by (time, id).
  events: NetworkEvent[];
  coverage?: NetworkCoverage;
  olderTruncated: boolean;
  capped: boolean;
  error?: string;
  since: Date;
}

export const maxEvents = 10_000;
export const storeIntervalMs = 5000;
export const gatewayIntervalMs = 15_000;

export const sourceLabel: Record<NetworkSource, string> = {
  [NetworkSource.UNSPECIFIED]: "telemetry",
  [NetworkSource.STORE]: "telemetry",
  [NetworkSource.GATEWAY_LOG]: "gateway log",
};

const ms = (e: NetworkEvent): number => (e.time ? timestampDate(e.time).getTime() : 0);

function compare(a: NetworkEvent, b: NetworkEvent): number {
  const d = ms(a) - ms(b);
  if (d !== 0) return d;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

export function merge(prev: NetworkState, res: ListNetworkEventsResponse, isFirst: boolean): NetworkState {
  const replace =
    isFirst || res.cursorReset || (prev.coverage !== undefined && res.coverage !== undefined && prev.coverage.source !== res.coverage.source);
  let events: NetworkEvent[];
  // An empty poll keeps the same array, so views keyed on it don't re-render every interval.
  if (!replace && res.events.length === 0) {
    events = prev.events;
  } else if (replace) {
    events = [...res.events];
  } else {
    const byId = new Map<string, NetworkEvent>();
    for (const e of prev.events) byId.set(e.id, e);
    for (const e of res.events) byId.set(e.id, e);
    events = [...byId.values()];
  }
  if (events !== prev.events) events.sort(compare);
  let capped = prev.capped;
  if (events.length > maxEvents) {
    events = events.slice(events.length - maxEvents);
    capped = true;
  }
  return {
    ...prev,
    events,
    coverage: res.coverage ?? prev.coverage,
    // A gateway-log poll re-reads the whole log, so its truncation is current on every response.
    olderTruncated: replace || res.coverage?.resumable === false ? res.truncated : prev.olderTruncated,
    capped,
    error: undefined,
  };
}

export interface NetworkPollerOptions {
  transport: Transport;
  sessionId: string;
  since: Date;
  sleep?: (ms: number) => Promise<void>;
  hidden?: () => boolean;
}

const defaultSleep = (t: number) => new Promise<void>((resolve) => setTimeout(resolve, t));
const defaultHidden = () => typeof document !== "undefined" && document.hidden;

export class NetworkPoller {
  private readonly client: Client<typeof SessionService>;
  private readonly sleep: (ms: number) => Promise<void>;
  private readonly hidden: () => boolean;
  private snapshot: NetworkState;
  private readonly listeners = new Set<() => void>();
  private started = false;
  private stopped = false;
  private first = true;
  private abort: AbortController | undefined;
  private readonly stopSignal: Promise<void>;
  private signalStop!: () => void;

  constructor(private readonly opts: NetworkPollerOptions) {
    this.client = createClient(SessionService, opts.transport);
    this.sleep = opts.sleep ?? defaultSleep;
    this.hidden = opts.hidden ?? defaultHidden;
    this.snapshot = { status: "loading", events: [], olderTruncated: false, capped: false, since: opts.since };
    this.stopSignal = new Promise<void>((resolve) => (this.signalStop = resolve));
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  getSnapshot = (): NetworkState => this.snapshot;

  start(): void {
    if (this.started) return;
    this.started = true;
    void this.run();
  }

  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    this.abort?.abort();
    this.signalStop();
    this.publish({ ...this.snapshot, status: "stopped" });
  }

  private publish(next: NetworkState): void {
    this.snapshot = next;
    for (const l of this.listeners) l();
  }

  private interval(): number {
    return this.snapshot.coverage?.resumable === false ? gatewayIntervalMs : storeIntervalMs;
  }

  private async run(): Promise<void> {
    let after = "";
    while (!this.stopped) {
      if (this.hidden()) {
        await Promise.race([this.sleep(this.interval()), this.stopSignal]);
        continue;
      }
      const sent = after;
      this.abort = new AbortController();
      try {
        const res = await this.client.listNetworkEvents(
          { sessionId: this.opts.sessionId, since: { case: "sinceTime", value: timestampFromDate(this.opts.since) }, after: sent },
          { signal: this.abort.signal },
        );
        if (this.stopped) return;
        this.publish({ ...merge(this.snapshot, res, this.first), status: "live" });
        this.first = false;
        after = res.coverage?.resumable && res.nextCursor ? res.nextCursor : "";
        if (sent !== "" && res.truncated) continue;
      } catch (err) {
        if (this.stopped) return;
        const ce = ConnectError.from(err);
        if (ce.code === Code.FailedPrecondition) {
          this.publish({ ...this.snapshot, status: "no-sandbox", error: undefined });
          return;
        }
        if (ce.code === Code.NotFound) {
          this.publish({ ...this.snapshot, status: "stopped", error: ce.rawMessage });
          return;
        }
        this.publish({ ...this.snapshot, error: ce.rawMessage });
      }
      await Promise.race([this.sleep(this.interval()), this.stopSignal]);
    }
  }
}

export function deniedByCall(
  events: NetworkEvent[],
  calls: Iterable<{ id: string; startedAt?: number; doneAt?: number }>,
  now: number,
): Map<string, NetworkEvent[]> {
  const denied = events.filter((e) => !e.allowed);
  const out = new Map<string, NetworkEvent[]>();
  if (denied.length === 0) return out;
  for (const c of calls) {
    const start = c.startedAt;
    if (start === undefined) continue;
    const end = c.doneAt ?? now;
    const hits = denied.filter((e) => {
      const t = ms(e);
      return start <= t && t <= end;
    });
    if (hits.length > 0) out.set(c.id, hits);
  }
  return out;
}

export interface Note {
  tone: "warn" | "error";
  text: string;
}

const count = (n: bigint, one: string) => `${n} ${n === 1n ? one : `${one}s`}`;

export function coverageNotes(s: NetworkState, fmt: (d: Date) => string): Note[] {
  const c = s.coverage;
  const notes: Note[] = [];
  const at = (t: Timestamp | undefined) => (t ? fmt(timestampDate(t)) : "an unknown time");
  if (c) {
    if (c.sourceBounded) {
      notes.push({
        tone: "warn",
        text: `Showing the gateway log, which keeps only recent lines${
          c.gatewayLogStart ? `; it starts at ${at(c.gatewayLogStart)}, so earlier decisions are missing` : ""
        }.`,
      });
    }
    if (!c.storeAvailable) {
      notes.push({ tone: "warn", text: "The telemetry store is off, so dropped and unattributed event counts are unknown." });
    }
    if (c.gatewayError) {
      notes.push({ tone: "error", text: "The gateway log could not be read, so its decisions are missing. See the server log." });
    }
    if (c.storeAvailable) {
      if (c.historyStart) {
        notes.push({ tone: "warn", text: `The telemetry store has nothing before ${at(c.historyStart)}.` });
      }
      const drop = (d: NetworkDrop | undefined, tone: Note["tone"], text: (n: string, when: string) => string) => {
        if (d && d.dropped > 0n) notes.push({ tone, text: text(count(d.dropped, "event"), at(d.since)) });
      };
      drop(c.droppedOwn, "error", (n, when) => `${n} from this sandbox were dropped since ${when} and are missing.`);
      drop(c.droppedGatewayWide, "warn", (n, when) => `${n} were dropped gateway-wide since ${when}; some may include this session.`);
      drop(c.droppedUnknown, "warn", (n, when) => `${n} were dropped for unnamed sandboxes since ${when}; some may include this session.`);
      if (c.unattributed > 0n) {
        notes.push({
          tone: "warn",
          text: `${count(c.unattributed, "decision")} had no sandbox and cannot be shown${
            c.unattributedFromDay ? ` (counted from ${c.unattributedFromDay}, UTC)` : ""
          }.`,
        });
      }
      if (c.pruned) {
        notes.push({ tone: "warn", text: "The window starts before the retention cutoff, so older events were deleted." });
      }
    }
  }
  if (s.olderTruncated) {
    notes.push({ tone: "warn", text: "Only the newest events are shown; older events in the window were left out." });
  }
  if (s.capped) {
    notes.push({ tone: "warn", text: `The page keeps only the newest ${maxEvents.toLocaleString("en-US")} events; older ones were dropped.` });
  }
  return notes;
}
