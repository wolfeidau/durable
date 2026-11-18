import { describe, expect, test } from "bun:test";
import { chip, defaultOpen, duration, isRunning, summary } from "./calls";
import { ApprovalStatus } from "./gen/durable/v1/durable_pb";
import type { CallEntry } from "./transcript";

const call = (extra: Partial<CallEntry> = {}): CallEntry => ({
  kind: "call",
  id: "c1",
  name: "bash",
  rewritten: false,
  isError: false,
  ...extra,
});
const approval = (status: ApprovalStatus) => ({ seq: 1n, status, decidedBy: "" });

describe("chip", () => {
  test("every state", () => {
    expect(chip(call({ approval: approval(ApprovalStatus.PENDING) }), undefined)).toEqual({ text: "waiting", tone: "tone-warn" });
    expect(chip(call({ approval: approval(ApprovalStatus.DECLINED) }), undefined)).toEqual({ text: "denied", tone: "tone-error" });
    expect(chip(call({ approval: approval(ApprovalStatus.CANCELLED) }), undefined)).toEqual({
      text: "cancelled",
      tone: "tone-cancelled",
    });
    expect(chip(call({ result: "ok" }), undefined)).toEqual({ text: "done", tone: "tone-ok" });
    expect(chip(call({ result: "bad", isError: true }), undefined)).toEqual({ text: "error", tone: "tone-error" });
    expect(chip(call(), undefined)).toEqual({ text: "running", tone: "tone-link" });
    expect(chip(call(), "out")).toEqual({ text: "running", tone: "tone-link pulse" });
    expect(chip(call({ approval: approval(ApprovalStatus.ALLOWED), result: "ok" }), undefined).text).toBe("done");
  });
});

describe("chip durations", () => {
  const timed = { startedAt: 1000, doneAt: 3100 };

  test("finished calls show their duration", () => {
    expect(chip(call({ result: "ok", ...timed }), undefined).text).toBe("done 2.1s");
    expect(chip(call({ result: "bad", isError: true, startedAt: 1000, doneAt: 1400 }), undefined).text).toBe("error 400ms");
  });

  test("a running call shows elapsed time", () => {
    expect(chip(call({ startedAt: 1000 }), undefined, 4000)).toEqual({ text: "running 3.0s", tone: "tone-link" });
    expect(chip(call({ startedAt: 1000 }), "out", 14000).text).toBe("running 13s");
  });

  test("missing timestamps show no time", () => {
    expect(chip(call({ result: "ok", startedAt: 1000 }), undefined).text).toBe("done");
    expect(chip(call({ result: "ok", doneAt: 1000 }), undefined).text).toBe("done");
    expect(chip(call(), undefined, 5000).text).toBe("running");
  });

  test("approval states carry no time", () => {
    expect(chip(call({ approval: approval(ApprovalStatus.PENDING), startedAt: 1 }), undefined, 9).text).toBe("waiting");
  });
});

describe("duration", () => {
  test("formats by size", () => {
    expect(duration(0)).toBe("0ms");
    expect(duration(33)).toBe("33ms");
    expect(duration(999.6)).toBe("1.0s");
    expect(duration(1000)).toBe("1.0s");
    expect(duration(2140)).toBe("2.1s");
    expect(duration(12400)).toBe("12s");
    expect(duration(65000)).toBe("1m 05s");
    expect(duration(-5)).toBe("0ms");
  });
});

describe("isRunning", () => {
  test("only an unfinished, undecided call runs", () => {
    expect(isRunning(call())).toBe(true);
    expect(isRunning(call({ result: "ok" }))).toBe(false);
    expect(isRunning(call({ approval: approval(ApprovalStatus.PENDING) }))).toBe(false);
    expect(isRunning(call({ approval: approval(ApprovalStatus.ALLOWED) }))).toBe(true);
  });
});

describe("summary", () => {
  const input = (v: unknown) => call({ input: JSON.stringify(v) });

  test("key priority", () => {
    expect(summary(input({ path: "/a", command: "ls" }))).toBe("ls");
    expect(summary(input({ path: "/a", n: 1 }))).toBe("/a");
    expect(summary(input({ url: "u", query: "q" }))).toBe("u");
    expect(summary(input({ n: 1, other: "first", more: "second" }))).toBe("first");
    expect(summary(input({ command: 5, path: "/p" }))).toBe("/p");
  });

  test("collapses whitespace", () => {
    expect(summary(input({ command: "a \n\t b   c\n" }))).toBe("a b c");
  });

  test("truncates at 80 characters", () => {
    expect(summary(input({ command: "x".repeat(80) }))).toBe("x".repeat(80));
    expect(summary(input({ command: "x".repeat(81) }))).toBe(`${"x".repeat(80)}…`);
  });

  test("empty when there is nothing to show", () => {
    expect(summary(call({ input: '{"command":"ls"}', rewritten: true }))).toBe("");
    expect(summary(call())).toBe("");
    expect(summary(call({ input: "not json" }))).toBe("");
    expect(summary(input({ n: 1 }))).toBe("");
    expect(summary(input(["a"]))).toBe("");
    expect(summary(call({ input: "null" }))).toBe("");
  });
});

describe("defaultOpen", () => {
  test("open while running or pending, on live output and on errors", () => {
    expect(defaultOpen(call(), undefined)).toBe(true);
    expect(defaultOpen(call({ approval: approval(ApprovalStatus.PENDING) }), undefined)).toBe(true);
    expect(defaultOpen(call({ result: "ok" }), "live")).toBe(true);
    expect(defaultOpen(call({ result: "bad", isError: true }), undefined)).toBe(true);
  });

  test("closed once done, denied or cancelled", () => {
    expect(defaultOpen(call({ result: "ok" }), undefined)).toBe(false);
    expect(defaultOpen(call({ approval: approval(ApprovalStatus.DECLINED), result: "no", isError: true }), undefined)).toBe(false);
    expect(defaultOpen(call({ approval: approval(ApprovalStatus.DECLINED) }), undefined)).toBe(false);
    expect(defaultOpen(call({ approval: approval(ApprovalStatus.CANCELLED) }), undefined)).toBe(false);
  });
});
