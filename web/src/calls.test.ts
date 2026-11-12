import { describe, expect, test } from "bun:test";
import { chip, defaultOpen, summary } from "./calls";
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
