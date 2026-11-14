import { describe, expect, test } from "bun:test";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { EventKind, EventSchema, Role, SubmissionStatus, type Event } from "./gen/durable/v1/durable_pb";
import { buildReport, compact, money, percentile, summarize, turnFooter } from "./usage";

let n = 0n;
const ev = (ms: number | undefined, init: MessageInitShape<typeof EventSchema>): Event =>
  create(EventSchema, { seq: ++n, submission: 1n, ...(ms === undefined ? {} : { at: timestampFromMs(ms) }), ...init });

const reply = (usage?: Record<string, unknown>, extra: Record<string, unknown> = {}, sub = 1n) =>
  ev(undefined, { kind: EventKind.MESSAGE, submission: sub, message: { role: Role.ASSISTANT, text: "hi", usage, ...extra } });
const status = (ms: number, s: SubmissionStatus, sub = 1n) => ev(ms, { kind: EventKind.SUBMISSION, status: s, submission: sub });
const started = (ms: number | undefined, id: string, name = "bash") => ev(ms, { kind: EventKind.CALL_STARTED, call: { id, name } });
const done = (ms: number | undefined, id: string, name = "bash", isError = false) =>
  ev(ms, { kind: EventKind.CALL_DONE, call: { id, name }, isError });

describe("percentile", () => {
  test("nearest rank", () => {
    const xs = Array.from({ length: 20 }, (_, i) => i + 1);
    expect(percentile(xs, 0.95)).toBe(19);
    expect(percentile([5, 1, 3], 0.95)).toBe(5);
    expect(percentile([7], 0.95)).toBe(7);
    expect(percentile([], 0.95)).toBeUndefined();
  });
});

describe("summarize", () => {
  test("no events is all unknown", () => {
    const s = summarize([]);
    expect([s.cost, s.cacheHit, s.contextFraction]).toEqual([undefined, undefined, undefined]);
    expect(s.unknownCost).toBe(0);
  });

  test("old sessions without usage have unknown cost", () => {
    const s = summarize([reply(undefined)]);
    expect(s.cost).toBeUndefined();
    expect(s.unknownCost).toBe(1);
  });

  test("mixes provider, estimate and unknown costs", () => {
    const s = summarize([
      reply({ costUsd: 0.01, costSource: "provider" }),
      reply({ costUsd: 0.005, costSource: "estimate" }),
      reply({ costUsd: 0, costSource: "" }),
    ]);
    expect(s.cost).toBeCloseTo(0.015);
    expect(s.estimated).toBe(true);
    expect(s.unknownCost).toBe(1);
    expect(summarize([reply({ costUsd: 0.01, costSource: "provider" })]).estimated).toBe(false);
  });

  test("sums tokens and computes the cache-hit ratio", () => {
    const s = summarize([
      reply({ inputTokens: 100n, cacheReadTokens: 300n, cacheWriteTokens: 100n, outputTokens: 50n }),
      reply({ inputTokens: 0n, cacheReadTokens: 100n, outputTokens: 10n }),
    ]);
    expect(s).toMatchObject({ input: 100, cacheRead: 400, cacheWrite: 100, output: 60 });
    expect(s.cacheHit).toBeCloseTo(400 / 600);
    expect(summarize([reply({ outputTokens: 5n })]).cacheHit).toBeUndefined();
  });

  test("context uses the latest size and latest known window", () => {
    const s = summarize([
      reply({ contextWindow: 1_000_000n }, { contextTokens: 100n }),
      reply({ contextWindow: 0n }, { contextTokens: 420_000n }),
    ]);
    expect(s.contextTokens).toBe(420_000);
    expect(s.contextWindow).toBe(1_000_000);
    expect(s.contextFraction).toBeCloseTo(0.42);
    expect(summarize([reply({}, { contextTokens: 5n })]).contextFraction).toBeUndefined();
    expect(summarize([reply({ contextWindow: 9n })]).contextFraction).toBeUndefined();
  });
});

describe("buildReport", () => {
  test("tool durations come from call_started to call_done", () => {
    const r = buildReport([
      started(1000, "a"),
      done(1100, "a"),
      started(2000, "b"),
      done(2300, "b", "bash", true),
      started(3000, "c", "read"),
      done(3050, "c", "read"),
    ]);
    expect(r.tools[0]).toEqual({ name: "bash", calls: 2, errors: 1, total: 400, avg: 200, p95: 300, max: 300 });
    expect(r.tools[1]).toMatchObject({ name: "read", calls: 1, total: 50 });
  });

  test("skips calls missing a timestamp and leaves their stats unknown", () => {
    const r = buildReport([started(undefined, "a"), done(1000, "a"), done(2000, "b")]);
    expect(r.tools[0]).toEqual({ name: "bash", calls: 2, errors: 0, total: undefined, avg: undefined, p95: undefined, max: undefined });
  });

  test("approval wait is excluded because call_started follows the decision", () => {
    const r = buildReport([
      ev(1000, { kind: EventKind.APPROVAL_REQUESTED, call: { id: "a", name: "bash" }, approvalSeq: 1n }),
      ev(61_000, { kind: EventKind.APPROVAL_DECIDED, call: { id: "a", name: "bash" }, approvalSeq: 1n }),
      started(61_010, "a"),
      done(61_110, "a"),
    ]);
    expect(r.tools[0]!.total).toBe(100);
  });

  test("model stats", () => {
    const r = buildReport([
      reply({ durationMs: 1000n, outputTokens: 50n }),
      reply({ durationMs: 3000n, outputTokens: 150n }),
      reply({ durationMs: 0n, outputTokens: 99n }),
    ]);
    expect(r.model).toEqual({ replies: 3, avg: 2000, p95: 3000, tokensPerSecond: 50 });
    expect(buildReport([reply(undefined)]).model).toEqual({ replies: 0, avg: undefined, p95: undefined, tokensPerSecond: undefined });
  });

  test("turns have wall time, cost, calls, and model vs tool time", () => {
    const r = buildReport([
      status(10_000, SubmissionStatus.RUNNING),
      reply({ costUsd: 0.004, costSource: "estimate", durationMs: 2000n }),
      started(11_000, "a"),
      done(11_500, "a"),
      status(22_300, SubmissionStatus.DONE),
      status(30_000, SubmissionStatus.RUNNING, 2n),
      reply({ costSource: "" }, {}, 2n),
    ]);
    expect(r.turns[0]).toEqual({ seq: 1n, cost: 0.004, estimated: true, wallMs: 12_300, calls: 1, modelMs: 2000, toolMs: 500 });
    expect(r.turns[1]).toMatchObject({ seq: 2n, cost: undefined, wallMs: undefined, calls: 0, modelMs: undefined, toolMs: undefined });
  });
});

describe("format", () => {
  test("compact numbers", () => {
    expect(compact(420_000)).toBe("420K");
    expect(compact(1_000_000)).toBe("1M");
    expect(compact(1_500_000)).toBe("1.5M");
    expect(compact(999)).toBe("999");
  });

  test("money", () => {
    expect(money(0.0123)).toBe("$0.0123");
    expect(money(0.004, true)).toBe("~$0.0040");
    expect(money(undefined)).toBe("—");
    expect(money(12.345)).toBe("$12.35");
  });

  test("turn footer omits unknown parts", () => {
    expect(turnFooter({ seq: 1n, cost: 0.0042, estimated: false, wallMs: 12_300, calls: 5 })).toBe("$0.0042 · 12.3 s · 5 calls");
    expect(turnFooter({ seq: 1n, estimated: false, calls: 1 })).toBe("1 call");
    expect(turnFooter({ seq: 1n, estimated: false, calls: 0 })).toBe("");
  });
});
