import { Code, ConnectError, createClient, type Client, type Transport } from "@connectrpc/connect";
import {
  DeltaKind,
  EventKind,
  Role,
  SessionService,
  SubmissionStatus,
  type Delta,
  type Event,
} from "./gen/durable/v1/durable_pb";

export type WatchStatus = "connecting" | "live" | "reconnecting" | "auth-needed" | "stopped";

export interface WatchState {
  readonly status: WatchStatus;
  readonly epoch: string;
  readonly cursor: bigint;
  readonly events: readonly Event[];
  readonly gaps: number;
  readonly reconnects: number;
  readonly lastHeartbeat: number | undefined;
  readonly lastError?: string;
  // Live views only: the committed events stay authoritative.
  readonly partialText: ReadonlyMap<bigint, string>;
  readonly toolOutput: ReadonlyMap<string, string>;
}

type Patch = { -readonly [K in keyof WatchState]?: WatchState[K] };

export interface WatchOptions {
  transport: Transport;
  sessionId: string;
  getToken: () => string;
  sleep?: (ms: number) => Promise<void>;
}

export const initialBackoffMs = 1000;
export const maxBackoffMs = 10_000;

const defaultSleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

const terminal = new Set([SubmissionStatus.DONE, SubmissionStatus.FAILED, SubmissionStatus.CANCELLED]);

function without<K, V>(m: ReadonlyMap<K, V>, key: K): ReadonlyMap<K, V> {
  if (!m.has(key)) return m;
  const next = new Map(m);
  next.delete(key);
  return next;
}

function withEntry<K, V>(m: ReadonlyMap<K, V>, key: K, value: V): ReadonlyMap<K, V> {
  return new Map(m).set(key, value);
}

export class WatchEngine {
  private readonly client: Client<typeof SessionService>;
  private readonly sleep: (ms: number) => Promise<void>;
  // The working state; subscribers see `snapshot`, published at most once per task so a replay renders once per chunk.
  private state: WatchState = {
    status: "connecting",
    epoch: "",
    cursor: 0n,
    events: [],
    gaps: 0,
    reconnects: 0,
    lastHeartbeat: undefined,
    partialText: new Map(),
    toolOutput: new Map(),
  };
  private snapshot: WatchState = this.state;
  // Appended in place and copied into the snapshot on flush, so a replay isn't quadratic.
  private events: Event[] = [];
  private eventsDirty = false;
  private flushScheduled = false;
  private readonly listeners = new Set<() => void>();
  private readonly resetSubmissions = new Set<bigint>();
  private started = false;
  private stopped = false;
  private abort: AbortController | undefined;
  private backoff = initialBackoffMs;
  private wake: (() => void) | undefined;
  private readonly stopSignal: Promise<void>;
  private signalStop!: () => void;

  constructor(private readonly opts: WatchOptions) {
    this.client = createClient(SessionService, opts.transport);
    this.sleep = opts.sleep ?? defaultSleep;
    this.stopSignal = new Promise<void>((resolve) => (this.signalStop = resolve));
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  getSnapshot = (): WatchState => this.snapshot;

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
    this.update({ status: "stopped" });
    this.flush();
  }

  resumeAuth(): void {
    this.wake?.();
  }

  private update(patch: Patch): void {
    this.state = { ...this.state, ...patch, ...(patch.status === "live" ? { lastError: undefined } : {}) };
    if (this.flushScheduled) return;
    this.flushScheduled = true;
    setTimeout(this.flush, 0);
  }

  private flush = (): void => {
    this.flushScheduled = false;
    const events = this.eventsDirty ? this.events.slice() : this.snapshot.events;
    this.eventsDirty = false;
    this.snapshot = { ...this.state, events };
    for (const l of this.listeners) l();
  };

  private async run(): Promise<void> {
    while (!this.stopped) {
      this.abort = new AbortController();
      let lastError: string | undefined;
      // Deltas missed while disconnected would leave a hole, so live views restart with the connection.
      this.resetSubmissions.clear();
      if (this.state.partialText.size || this.state.toolOutput.size) {
        this.update({ partialText: new Map(), toolOutput: new Map() });
      }
      try {
        const stream = this.client.watchEvents(
          { sessionId: this.opts.sessionId, epoch: this.state.epoch, afterSeq: this.state.cursor, includeDeltas: true },
          {
            signal: this.abort.signal,
            headers: { Authorization: `Bearer ${this.opts.getToken()}` },
            // The server sends headers once the watch holds the session, before any event or heartbeat.
            onHeader: () => {
              if (!this.stopped && this.state.status !== "live") this.update({ status: "live" });
            },
          },
        );
        for await (const r of stream) {
          if (this.stopped) return;
          this.backoff = initialBackoffMs;
          if (this.state.status !== "live") this.update({ status: "live" });
          switch (r.item.case) {
            case "event":
              this.applyEvent(r.item.value);
              break;
            case "heartbeat":
              this.update({ lastHeartbeat: Date.now() });
              break;
            case "delta":
              this.applyDelta(r.item.value);
              break;
          }
        }
      } catch (err) {
        if (this.stopped) return;
        const code = ConnectError.from(err).code;
        if (code === Code.FailedPrecondition) {
          this.resetAll();
          continue;
        }
        if (code === Code.Unauthenticated) {
          this.update({ status: "auth-needed" });
          await Promise.race([new Promise<void>((resolve) => (this.wake = resolve)), this.stopSignal]);
          this.wake = undefined;
          if (!this.stopped) this.update({ status: "connecting" });
          continue;
        }
        lastError = ConnectError.from(err).rawMessage;
      }
      if (this.stopped) return;
      this.update({ status: "reconnecting", reconnects: this.state.reconnects + 1, lastError });
      const delay = this.backoff;
      this.backoff = Math.min(this.backoff * 2, maxBackoffMs);
      await Promise.race([this.sleep(delay), this.stopSignal]);
    }
  }

  private resetAll(): void {
    this.resetSubmissions.clear();
    this.events = [];
    this.eventsDirty = true;
    this.update({
      status: "connecting",
      epoch: "",
      cursor: 0n,
      partialText: new Map(),
      toolOutput: new Map(),
    });
  }

  private applyEvent(e: Event): void {
    const { cursor } = this.state;
    if (e.seq <= cursor) return;
    this.events.push(e);
    this.eventsDirty = true;
    const patch: Patch = {
      cursor: e.seq,
      epoch: e.epoch,
    };
    if (e.seq !== cursor + 1n) patch.gaps = this.state.gaps + 1;

    const assistant = e.kind === EventKind.MESSAGE && e.message?.role === Role.ASSISTANT;
    if (assistant || terminal.has(e.status)) {
      patch.partialText = without(this.state.partialText, e.submission);
    }
    if (e.kind === EventKind.CALL_DONE && e.call) {
      patch.toolOutput = without(this.state.toolOutput, e.call.id);
    }
    this.update(patch);
  }

  private applyDelta(d: Delta): void {
    switch (d.kind) {
      case DeltaKind.RESET:
        this.resetSubmissions.add(d.submission);
        this.update({ partialText: withEntry(this.state.partialText, d.submission, "") });
        break;
      case DeltaKind.TEXT:
        // Attached mid-reply, the text would be a fragment.
        if (!this.resetSubmissions.has(d.submission)) return;
        this.update({
          partialText: withEntry(this.state.partialText, d.submission, (this.state.partialText.get(d.submission) ?? "") + d.text),
        });
        break;
      case DeltaKind.TOOL_OUTPUT:
        this.update({
          toolOutput: withEntry(this.state.toolOutput, d.callId, (this.state.toolOutput.get(d.callId) ?? "") + d.text),
        });
        break;
    }
  }
}
