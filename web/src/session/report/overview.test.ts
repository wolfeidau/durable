import { describe, expect, test } from "bun:test";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { EventKind, EventSchema, Role, SubmissionStatus, type Event } from "../../gen/durable/v1/durable_pb";
import { buildOverview, errorLine, firstLine, segments, tokenSplit } from "./overview";
import { buildTranscript } from "../transcript/transcript";
import { buildReport, summarize } from "./usage";

let n = 0n;
const ev = (ms: number | undefined, init: MessageInitShape<typeof EventSchema>): Event =>
  create(EventSchema, { seq: ++n, submission: 1n, ...(ms === undefined ? {} : { at: timestampFromMs(ms) }), ...init });

const status = (ms: number | undefined, s: SubmissionStatus, sub = 1n) => ev(ms, { kind: EventKind.SUBMISSION, status: s, submission: sub });
const user = (text: string, sub = 1n) => ev(undefined, { kind: EventKind.MESSAGE, submission: sub, message: { role: Role.USER, text } });
const reply = (usage?: Record<string, unknown>, extra: Record<string, unknown> = {}, sub = 1n) =>
  ev(undefined, { kind: EventKind.MESSAGE, submission: sub, message: { role: Role.ASSISTANT, text: "hi", usage, ...extra } });
const call = (id: string, name: string, input: string, sub = 1n) =>
  ev(undefined, {
    kind: EventKind.MESSAGE,
    submission: sub,
    message: { role: Role.ASSISTANT, toolCalls: [{ id, name, input: new TextEncoder().encode(input) }] },
  });
const started = (ms: number | undefined, id: string, sub = 1n) => ev(ms, { kind: EventKind.CALL_STARTED, submission: sub, call: { id, name: "bash" } });
const done = (ms: number | undefined, id: string, isError = false, sub = 1n) =>
  ev(ms, { kind: EventKind.CALL_DONE, submission: sub, call: { id, name: "bash" }, isError });
const result = (id: string, text: string, isError: boolean, sub = 1n) =>
  ev(undefined, { kind: EventKind.MESSAGE, submission: sub, message: { role: Role.TOOL, toolCallId: id, text, isError } });
const compacted = (sub = 1n) => ev(undefined, { kind: EventKind.COMPACTED, submission: sub });

const overview = (events: Event[]) => buildOverview(events, buildTranscript(events), buildReport(events));

describe("firstLine", () => {
  test("skips blank lines and trims", () => {
    expect(firstLine("\n  \n  hello  \nworld")).toBe("hello");
    expect(firstLine("")).toBe("");
  });
});

describe("segments", () => {
  test("splits wall into model, tool and other", () => {
    expect(segments(10_000, 2000, 3000)).toEqual({ segModel: 2000, segTool: 3000, segOther: 5000, totalMs: 10_000 });
  });

  test("clamps model + tool that exceed the wall", () => {
    expect(segments(1000, 800, 800)).toEqual({ segModel: 800, segTool: 200, segOther: 0, totalMs: 1000 });
    expect(segments(500, 800, 800)).toEqual({ segModel: 500, segTool: 0, segOther: 0, totalMs: 500 });
  });

  test("an unknown wall uses model + tool", () => {
    expect(segments(undefined, 2000, 500)).toEqual({ segModel: 2000, segTool: 500, segOther: 0, totalMs: 2500 });
    expect(segments(undefined)).toEqual({ segModel: 0, segTool: 0, segOther: 0, totalMs: 0 });
  });
});

describe("buildOverview", () => {
  test("an empty session has nothing", () => {
    const o = overview([]);
    expect(o).toMatchObject({ turns: [], calls: [], slowest: [], failed: [], context: [], compactions: [], errors: 0, totalCalls: 0, wallMs: undefined });
  });

  test("turn bars carry the prompt's first line, split, cost and status", () => {
    const o = overview([
      status(10_000, SubmissionStatus.RUNNING),
      user("\nfix the bug\nplease"),
      reply({ costUsd: 0.004, costSource: "estimate", durationMs: 2000n }),
      call("a", "bash", '{"command":"ls"}'),
      started(11_000, "a"),
      done(11_500, "a"),
      result("a", "ok", false),
      status(20_000, SubmissionStatus.DONE),
    ]);
    expect(o.turns).toHaveLength(1);
    expect(o.turns[0]).toMatchObject({
      n: 1,
      prompt: "fix the bug",
      status: SubmissionStatus.DONE,
      errors: 0,
      calls: 1,
      cost: 0.004,
      estimated: true,
      wallMs: 10_000,
      segModel: 2000,
      segTool: 500,
      segOther: 7500,
      totalMs: 10_000,
    });
    expect(o.wallMs).toBe(10_000);
  });

  test("a failed turn and a cancelled turn keep their status, with unknown cost and wall", () => {
    const o = overview([
      status(1000, SubmissionStatus.RUNNING),
      user("one"),
      status(2000, SubmissionStatus.FAILED),
      status(3000, SubmissionStatus.RUNNING, 2n),
      user("two", 2n),
      reply({ costSource: "" }, {}, 2n),
      status(undefined, SubmissionStatus.CANCELLED, 2n),
    ]);
    expect(o.turns.map((t) => t.status)).toEqual([SubmissionStatus.FAILED, SubmissionStatus.CANCELLED]);
    expect(o.turns[1]).toMatchObject({ n: 2, cost: undefined, wallMs: undefined, totalMs: 0 });
    expect(o.wallMs).toBe(1000);
  });

  test("calls get durations, previews and the first error line; failed is newest first", () => {
    const o = overview([
      status(0, SubmissionStatus.RUNNING),
      user("go"),
      call("a", "bash", '{"command":"sleep 1"}'),
      started(1000, "a"),
      done(2000, "a"),
      result("a", "fine", false),
      call("b", "bash", '{"command":"false"}'),
      started(3000, "b"),
      done(3100, "b", true),
      result("b", "\nexit status 1\nmore", true),
      call("c", "bash", "{}"),
      started(undefined, "c"),
      done(4000, "c", true),
      result("c", "", true),
      call("d", "bash", '{"command":"later"}'),
      status(5000, SubmissionStatus.DONE),
    ]);
    expect(o.calls.map((c) => c.id)).toEqual(["a", "b", "c"]);
    expect(o.calls[0]).toMatchObject({ name: "bash", preview: "sleep 1", ms: 1000, isError: false, error: "", n: 1 });
    expect(o.calls[1]).toMatchObject({ ms: 100, error: "exit status 1" });
    expect(o.calls[2]).toMatchObject({ ms: undefined, error: "" });
    expect(o.failed.map((c) => c.id)).toEqual(["c", "b"]);
    expect(o.slowest.map((c) => c.id)).toEqual(["a", "b"]);
    expect(o.turns[0]!.errors).toBe(2);
    expect(o.errors).toBe(2);
    expect(o.totalCalls).toBe(3);
  });

  test("the slowest list is capped at five", () => {
    const events: Event[] = [];
    for (let i = 0; i < 8; i++) {
      events.push(call(`c${i}`, "bash", "{}"), started(i * 10_000, `c${i}`), done(i * 10_000 + (i + 1) * 100, `c${i}`), result(`c${i}`, "", false));
    }
    expect(overview(events).slowest.map((c) => c.id)).toEqual(["c7", "c6", "c5", "c4", "c3"]);
  });

  test("context points carry the window forward and compactions sit between replies", () => {
    const o = overview([
      status(0, SubmissionStatus.RUNNING),
      reply({ contextWindow: 1000n }, { contextTokens: 100n }),
      reply({}, { contextTokens: 0n }),
      reply({ contextWindow: 0n }, { contextTokens: 600n }),
      compacted(),
      reply(undefined, { contextTokens: 50n }),
    ]);
    expect(o.context.map((p) => [p.index, p.tokens, p.window])).toEqual([
      [0, 100, 1000],
      [1, 600, 1000],
      [2, 50, 1000],
    ]);
    expect(o.compactions).toEqual([{ at: 2, seq: 1n, n: 1 }]);
  });

  test("replies without usage or context tokens add no points", () => {
    const o = overview([reply(undefined), reply({})]);
    expect(o.context).toEqual([]);
    expect(o.compactions).toEqual([]);
  });
});

describe("tokenSplit", () => {
  test("lists the four parts in order", () => {
    const s = summarize([reply({ inputTokens: 1n, cacheReadTokens: 2n, cacheWriteTokens: 3n, outputTokens: 4n })]);
    expect(tokenSplit(s).map((p) => [p.key, p.tokens])).toEqual([
      ["input", 1],
      ["cacheRead", 2],
      ["cacheWrite", 3],
      ["output", 4],
    ]);
    expect(tokenSplit(summarize([])).every((p) => p.tokens === 0)).toBe(true);
  });
});

describe("errorLine", () => {
  test("replaces the bash exit prefix with the code and the first output line", () => {
    expect(errorLine("command exited with code 1; output:\nerror: unexpected argument\n\nUsage: x")).toBe("exit 1: error: unexpected argument");
    expect(errorLine("command exited with code 2; output:\n")).toBe("exit 2");
  });

  test("other errors use their first line", () => {
    expect(errorLine("\npermission denied\nmore")).toBe("permission denied");
    expect(errorLine("")).toBe("");
  });
});
