import { EventKind, Role, SubmissionStatus, type Event } from "../../gen/durable/v1/durable_pb";
import { summary } from "../transcript/calls";
import type { Report, Summary } from "./usage";
import type { Turn } from "../transcript/transcript";

export interface TurnBar {
  seq: bigint;
  // Position in the session, from 1.
  n: number;
  prompt: string;
  status: SubmissionStatus;
  errors: number;
  calls: number;
  cost?: number;
  estimated: boolean;
  wallMs?: number;
  // Raw figures, for the tooltip; the segments below are clamped to fit the bar.
  modelMs?: number;
  toolMs?: number;
  segModel: number;
  segTool: number;
  segOther: number;
  // The bar's length: wall time, or model + tool when the wall is unknown.
  totalMs: number;
}

export interface CallRow {
  id: string;
  seq: bigint;
  n: number;
  name: string;
  preview: string;
  ms?: number;
  isError: boolean;
  error: string;
}

export interface ContextPoint {
  index: number;
  seq: bigint;
  n: number;
  tokens: number;
  window?: number;
}

export interface Compaction {
  // The number of context points before it, so it sits between two replies.
  at: number;
  seq: bigint;
  n: number;
}

export interface Overview {
  turns: TurnBar[];
  calls: CallRow[];
  slowest: CallRow[];
  // Newest first.
  failed: CallRow[];
  context: ContextPoint[];
  compactions: Compaction[];
  errors: number;
  totalCalls: number;
  wallMs?: number;
}

export const slowestCount = 5;

export function firstLine(text: string): string {
  return text.split("\n").find((l) => l.trim() !== "")?.trim() ?? "";
}

const exitPrefix = /^command exited with code (\d+); output:\s*/;

// The bash tool prefixes failures with its exit code, which would make every row read the same.
export function errorLine(text: string): string {
  const m = exitPrefix.exec(text);
  if (!m) return firstLine(text);
  const rest = firstLine(text.slice(m[0].length));
  return rest ? `exit ${m[1]}: ${rest}` : `exit ${m[1]}`;
}

export function segments(wallMs: number | undefined, modelMs = 0, toolMs = 0) {
  if (wallMs === undefined) return { segModel: modelMs, segTool: toolMs, segOther: 0, totalMs: modelMs + toolMs };
  const segModel = Math.min(modelMs, wallMs);
  const segTool = Math.min(toolMs, wallMs - segModel);
  return { segModel, segTool, segOther: wallMs - segModel - segTool, totalMs: wallMs };
}

export function buildOverview(events: readonly Event[], turns: readonly Turn[], report: Report): Overview {
  const bySeq = new Map(turns.map((t) => [t.seq, t]));
  const numbers = new Map(report.turns.map((t, i) => [t.seq, i + 1]));
  const calls: CallRow[] = [];
  const bars = report.turns.map((row): TurnBar => {
    const t = bySeq.get(row.seq);
    const n = numbers.get(row.seq)!;
    let errors = 0;
    for (const e of t?.entries ?? []) {
      if (e.kind !== "call") continue;
      if (e.isError) errors++;
      if (e.result === undefined && e.doneAt === undefined) continue;
      calls.push({
        id: e.id,
        seq: row.seq,
        n,
        name: e.name,
        preview: summary(e),
        ms: e.startedAt !== undefined && e.doneAt !== undefined ? Math.max(0, e.doneAt - e.startedAt) : undefined,
        isError: e.isError,
        error: e.isError ? errorLine(e.result ?? "") : "",
      });
    }
    const user = t?.entries.find((e) => e.kind === "user");
    return {
      seq: row.seq,
      n,
      prompt: user && "text" in user ? firstLine(user.text) : "",
      status: t?.status ?? SubmissionStatus.UNSPECIFIED,
      errors,
      calls: row.calls,
      cost: row.cost,
      estimated: row.estimated,
      wallMs: row.wallMs,
      modelMs: row.modelMs,
      toolMs: row.toolMs,
      ...segments(row.wallMs, row.modelMs, row.toolMs),
    };
  });

  const context: ContextPoint[] = [];
  const compactions: Compaction[] = [];
  let window: number | undefined;
  for (const e of events) {
    if (e.kind === EventKind.COMPACTED) {
      compactions.push({ at: context.length, seq: e.submission, n: numbers.get(e.submission) ?? 0 });
      continue;
    }
    const m = e.message;
    if (e.kind !== EventKind.MESSAGE || m?.role !== Role.ASSISTANT) continue;
    if (m.usage && m.usage.contextWindow > 0n) window = Number(m.usage.contextWindow);
    if (m.contextTokens > 0n) {
      context.push({ index: context.length, seq: e.submission, n: numbers.get(e.submission) ?? 0, tokens: Number(m.contextTokens), window });
    }
  }

  const walls = bars.flatMap((b) => (b.wallMs === undefined ? [] : [b.wallMs]));
  return {
    turns: bars,
    calls,
    slowest: calls
      .filter((c) => c.ms !== undefined)
      .sort((a, b) => b.ms! - a.ms!)
      .slice(0, slowestCount),
    failed: calls.filter((c) => c.isError).reverse(),
    context,
    compactions,
    errors: report.tools.reduce((a, t) => a + t.errors, 0),
    totalCalls: report.tools.reduce((a, t) => a + t.calls, 0),
    wallMs: walls.length ? walls.reduce((a, b) => a + b, 0) : undefined,
  };
}

export interface SplitPart {
  key: "input" | "cacheRead" | "cacheWrite" | "output";
  label: string;
  tokens: number;
}

export function tokenSplit(s: Summary): SplitPart[] {
  return [
    { key: "input", label: "Uncached input", tokens: s.input },
    { key: "cacheRead", label: "Cache read", tokens: s.cacheRead },
    { key: "cacheWrite", label: "Cache write", tokens: s.cacheWrite },
    { key: "output", label: "Output", tokens: s.output },
  ];
}
