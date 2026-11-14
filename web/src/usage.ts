import { timestampMs } from "@bufbuild/protobuf/wkt";
import { EventKind, Role, SubmissionStatus, type Event } from "./gen/durable/v1/durable_pb";

export interface Summary {
  // Undefined when no reply has a known cost.
  cost?: number;
  estimated: boolean;
  unknownCost: number;
  input: number;
  cacheRead: number;
  cacheWrite: number;
  output: number;
  cacheHit?: number;
  contextTokens?: number;
  contextWindow?: number;
  contextFraction?: number;
}

export interface ToolRow {
  name: string;
  calls: number;
  errors: number;
  total?: number;
  avg?: number;
  p95?: number;
  max?: number;
}

export interface ModelRow {
  replies: number;
  avg?: number;
  p95?: number;
  tokensPerSecond?: number;
}

export interface TurnRow {
  seq: bigint;
  cost?: number;
  estimated: boolean;
  wallMs?: number;
  calls: number;
  modelMs?: number;
  toolMs?: number;
}

export interface Report {
  tools: ToolRow[];
  model: ModelRow;
  turns: TurnRow[];
}

const at = (e: Event): number | undefined => (e.at ? timestampMs(e.at) : undefined);

// Nearest-rank: the value at rank ceil(p * n) of the sorted samples.
export function percentile(samples: readonly number[], p: number): number | undefined {
  if (samples.length === 0) return undefined;
  const sorted = [...samples].sort((a, b) => a - b);
  return sorted[Math.max(1, Math.ceil(p * sorted.length)) - 1];
}

const sum = (xs: readonly number[]): number | undefined => (xs.length ? xs.reduce((a, b) => a + b, 0) : undefined);

const isTerminal = (s: SubmissionStatus) =>
  s === SubmissionStatus.DONE || s === SubmissionStatus.FAILED || s === SubmissionStatus.CANCELLED;

export function summarize(events: readonly Event[]): Summary {
  let cost = 0;
  let known = 0;
  const s: Summary = { estimated: false, unknownCost: 0, input: 0, cacheRead: 0, cacheWrite: 0, output: 0 };
  let ctx = 0;
  let win = 0;
  for (const e of events) {
    const m = e.message;
    if (e.kind !== EventKind.MESSAGE || !m || m.role !== Role.ASSISTANT) continue;
    const u = m.usage;
    if (u?.costSource) {
      cost += u.costUsd;
      known++;
      if (u.costSource === "estimate") s.estimated = true;
    } else {
      s.unknownCost++;
    }
    if (u) {
      s.input += Number(u.inputTokens);
      s.cacheRead += Number(u.cacheReadTokens);
      s.cacheWrite += Number(u.cacheWriteTokens);
      s.output += Number(u.outputTokens);
      if (u.contextWindow > 0n) win = Number(u.contextWindow);
    }
    if (m.contextTokens > 0n) ctx = Number(m.contextTokens);
  }
  if (known > 0) s.cost = cost;
  const denom = s.input + s.cacheRead + s.cacheWrite;
  if (denom > 0) s.cacheHit = s.cacheRead / denom;
  if (ctx > 0) s.contextTokens = ctx;
  if (win > 0) s.contextWindow = win;
  if (ctx > 0 && win > 0) s.contextFraction = ctx / win;
  return s;
}

interface TurnAcc {
  cost: number;
  known: number;
  estimated: boolean;
  start?: number;
  end?: number;
  calls: number;
  model: number[];
  tool: number[];
}

export function buildReport(events: readonly Event[]): Report {
  // call_started commits after the approval decision (the harness decides, then commits started), so approval wait is not in these durations.
  const started = new Map<string, number>();
  const tools = new Map<string, { calls: number; errors: number; ms: number[] }>();
  const turns = new Map<bigint, TurnAcc>();
  const replyMs: number[] = [];
  let replies = 0;
  let outTokens = 0;
  let outMs = 0;

  const turn = (seq: bigint) => {
    let t = turns.get(seq);
    if (!t) turns.set(seq, (t = { cost: 0, known: 0, estimated: false, calls: 0, model: [], tool: [] }));
    return t;
  };

  for (const e of events) {
    const t = turn(e.submission);
    const when = at(e);
    switch (e.kind) {
      case EventKind.SUBMISSION:
        if (e.status === SubmissionStatus.RUNNING && t.start === undefined) t.start = when;
        else if (isTerminal(e.status)) t.end = when;
        break;
      case EventKind.MESSAGE: {
        const m = e.message;
        const u = m?.usage;
        if (m?.role !== Role.ASSISTANT || !u) break;
        replies++;
        if (u.costSource) {
          t.cost += u.costUsd;
          t.known++;
          if (u.costSource === "estimate") t.estimated = true;
        }
        const ms = Number(u.durationMs);
        if (ms > 0) {
          replyMs.push(ms);
          outMs += ms;
          outTokens += Number(u.outputTokens);
          t.model.push(ms);
        }
        break;
      }
      case EventKind.CALL_STARTED:
        if (e.call && when !== undefined) started.set(`${e.submission}/${e.call.id}`, when);
        break;
      case EventKind.CALL_DONE: {
        if (!e.call) break;
        const row = tools.get(e.call.name) ?? { calls: 0, errors: 0, ms: [] };
        tools.set(e.call.name, row);
        row.calls++;
        if (e.isError) row.errors++;
        t.calls++;
        const key = `${e.submission}/${e.call.id}`;
        const from = started.get(key);
        started.delete(key);
        if (from !== undefined && when !== undefined) {
          const ms = Math.max(0, when - from);
          row.ms.push(ms);
          t.tool.push(ms);
        }
        break;
      }
    }
  }

  const toolRows = [...tools].map(([name, r]): ToolRow => {
    const total = sum(r.ms);
    return {
      name,
      calls: r.calls,
      errors: r.errors,
      total,
      avg: total === undefined ? undefined : total / r.ms.length,
      p95: percentile(r.ms, 0.95),
      max: r.ms.length ? Math.max(...r.ms) : undefined,
    };
  });
  toolRows.sort((a, b) => (b.total ?? -1) - (a.total ?? -1) || a.name.localeCompare(b.name));

  const modelTotal = sum(replyMs);
  return {
    tools: toolRows,
    model: {
      replies,
      avg: modelTotal === undefined ? undefined : modelTotal / replyMs.length,
      p95: percentile(replyMs, 0.95),
      tokensPerSecond: outMs > 0 ? outTokens / (outMs / 1000) : undefined,
    },
    turns: [...turns]
      .filter(([, t]) => t.start !== undefined || t.calls > 0 || t.model.length > 0 || t.known > 0)
      .map(([seq, t]) => ({
        seq,
        cost: t.known > 0 ? t.cost : undefined,
        estimated: t.estimated,
        wallMs: t.start !== undefined && t.end !== undefined ? Math.max(0, t.end - t.start) : undefined,
        calls: t.calls,
        modelMs: sum(t.model),
        toolMs: sum(t.tool),
      })),
  };
}

export function compact(n: number): string {
  const units: [number, string][] = [
    [1e9, "B"],
    [1e6, "M"],
    [1e3, "K"],
  ];
  for (const [size, suffix] of units) {
    if (n >= size) {
      const v = n / size;
      return `${v >= 100 || Number.isInteger(v) ? Math.round(v) : v.toFixed(1)}${suffix}`;
    }
  }
  return String(Math.round(n));
}

export const dash = "—";

export function money(usd: number | undefined, estimated = false): string {
  if (usd === undefined) return dash;
  return `${estimated ? "~" : ""}$${usd.toFixed(usd < 1 ? 4 : 2)}`;
}

export function seconds(ms: number | undefined): string {
  return ms === undefined ? dash : `${(ms / 1000).toFixed(1)} s`;
}

// A small non-zero share reads "<1%", because "0%" looks like nothing is used.
export const percent = (f: number | undefined): string =>
  f === undefined ? dash : f > 0 && f < 0.005 ? "<1%" : `${Math.round(f * 100)}%`;

// "$0.0042 · 12.3 s · 5 calls", leaving out what is unknown.
export function turnFooter(t: TurnRow): string {
  const parts: string[] = [];
  if (t.cost !== undefined) parts.push(money(t.cost, t.estimated));
  if (t.wallMs !== undefined) parts.push(seconds(t.wallMs));
  if (t.calls > 0) parts.push(`${t.calls} ${t.calls === 1 ? "call" : "calls"}`);
  return parts.join(" · ");
}
