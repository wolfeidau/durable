import { describe, expect, test } from "bun:test";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  ApprovalStatus,
  EventKind,
  EventSchema,
  Role,
  SubmissionStatus,
  type Event,
} from "./gen/durable/v1/durable_pb";
import { buildTranscript, TranscriptBuilder, type Turn } from "./transcript";

let n = 0n;
const ev = (init: MessageInitShape<typeof EventSchema>): Event => create(EventSchema, { seq: ++n, submission: 1n, ...init });
const bytes = (s: string) => new TextEncoder().encode(s);

const sub = (status: SubmissionStatus, extra: MessageInitShape<typeof EventSchema> = {}) =>
  ev({ kind: EventKind.SUBMISSION, status, ...extra });
const msg = (role: Role, text: string, extra: Record<string, unknown> = {}) =>
  ev({ kind: EventKind.MESSAGE, message: { role, text, ...extra } });

describe("buildTranscript", () => {
  test("empty input gives no turns", () => {
    expect(buildTranscript([])).toEqual([]);
  });

  test("groups a user message, assistant text, and a tool call with its result", () => {
    const call = { id: "c1", name: "bash", input: bytes('{"cmd":"ls"}') };
    const turns = buildTranscript([
      sub(SubmissionStatus.QUEUED, { requestId: "r1" }),
      msg(Role.USER, "list files"),
      sub(SubmissionStatus.RUNNING),
      msg(Role.ASSISTANT, "sure", { toolCalls: [call] }),
      ev({ kind: EventKind.CALL_STARTED, call }),
      msg(Role.TOOL, "a.txt", { toolCallId: "c1" }),
      ev({ kind: EventKind.CALL_DONE, call: { id: "c1", name: "bash" } }),
      msg(Role.ASSISTANT, "found a.txt"),
      sub(SubmissionStatus.DONE),
    ]);
    expect(turns).toHaveLength(1);
    const t = turns[0]!;
    expect(t.requestId).toBe("r1");
    expect(t.status).toBe(SubmissionStatus.DONE);
    expect(t.entries.map((e) => e.kind)).toEqual(["user", "assistant", "call", "assistant"]);
    expect(t.entries[2]).toMatchObject({ name: "bash", input: '{"cmd":"ls"}', result: "a.txt", isError: false });
  });

  test("marks a failed tool result", () => {
    const call = { id: "c1", name: "bash", input: bytes("{}") };
    const [t] = buildTranscript([
      msg(Role.ASSISTANT, "", { toolCalls: [call] }),
      msg(Role.TOOL, "boom", { toolCallId: "c1", isError: true }),
      ev({ kind: EventKind.CALL_DONE, call: { id: "c1", name: "bash" }, isError: true }),
    ]);
    expect(t!.entries[0]).toMatchObject({ kind: "call", result: "boom", isError: true });
  });

  test("approval requested then decided", () => {
    const call = { id: "c1", name: "bash", input: bytes("{}") };
    const requested = [
      msg(Role.ASSISTANT, "", { toolCalls: [call] }),
      ev({ kind: EventKind.APPROVAL_REQUESTED, call, approvalSeq: 7n, approval: ApprovalStatus.PENDING }),
    ];
    const pending = buildTranscript(requested)[0]!.entries[0];
    expect(pending).toMatchObject({ approval: { seq: 7n, status: ApprovalStatus.PENDING } });

    const decided = buildTranscript([
      ...requested,
      ev({ kind: EventKind.APPROVAL_DECIDED, approvalSeq: 7n, approval: ApprovalStatus.ALLOWED, decidedBy: "web" }),
    ])[0]!.entries[0];
    expect(decided).toMatchObject({ approval: { seq: 7n, status: ApprovalStatus.ALLOWED, decidedBy: "web" } });
  });

  test("a rewritten call hides its input", () => {
    const call = { id: "c1", name: "bash", input: bytes("{}") };
    const [t] = buildTranscript([
      msg(Role.ASSISTANT, "", { toolCalls: [call] }),
      ev({ kind: EventKind.APPROVAL_REQUESTED, call: { id: "c1", name: "bash" }, rewritten: true, approvalSeq: 2n }),
      ev({ kind: EventKind.CALL_STARTED, call }),
    ]);
    expect(t!.entries[0]).toMatchObject({ rewritten: true, input: undefined });
  });

  test("failed and cancelled turns keep their error", () => {
    const turns = buildTranscript([
      sub(SubmissionStatus.FAILED, { error: "provider down" }),
      sub(SubmissionStatus.CANCELLED, { submission: 2n, error: "cancelled" }),
    ]);
    expect(turns.map((t) => [t.seq, t.status, t.error])).toEqual([
      [1n, SubmissionStatus.FAILED, "provider down"],
      [2n, SubmissionStatus.CANCELLED, "cancelled"],
    ]);
  });

  test("a harness note in the user's turn is a notice, not a user message", () => {
    const [turn] = buildTranscript([
      msg(Role.USER, "do it"),
      msg(Role.USER, "The user cancelled the request above.", { note: true }),
      sub(SubmissionStatus.CANCELLED),
    ]);
    expect(turn?.entries).toEqual([
      { kind: "user", text: "do it" },
      { kind: "notice", text: "The user cancelled the request above." },
    ]);
  });

  test("compaction events become notices", () => {
    const [t] = buildTranscript([
      ev({ kind: EventKind.COMPACTED }),
      ev({ kind: EventKind.COMPACTION_FAILED, error: "too big" }),
      ev({ kind: EventKind.CALL_REPLAYED, call: { id: "c1", name: "bash" } }),
    ]);
    expect(t!.entries).toEqual([
      { kind: "notice", text: "Earlier messages were compacted" },
      { kind: "notice", text: "Compaction failed: too big" },
      { kind: "notice", text: "bash ran again after a restart" },
    ]);
  });

  test("a turn queued without a request id has none", () => {
    const [t] = buildTranscript([sub(SubmissionStatus.QUEUED), msg(Role.USER, "hi")]);
    expect(t!.requestId).toBeUndefined();
    expect(t!.entries).toEqual([{ kind: "user", text: "hi" }]);
  });

  test("events missing their optional parts are ignored", () => {
    const turns = buildTranscript([
      ev({ kind: EventKind.MESSAGE }),
      ev({ kind: EventKind.CALL_STARTED }),
      ev({ kind: EventKind.CALL_DONE }),
      ev({ kind: EventKind.APPROVAL_REQUESTED }),
      ev({ kind: EventKind.APPROVAL_DECIDED, approvalSeq: 9n }),
      ev({ kind: EventKind.UNSPECIFIED }),
    ]);
    expect(turns).toHaveLength(1);
    expect(turns[0]!.entries).toEqual([]);
  });

  test("a call with empty input has no input", () => {
    const [t] = buildTranscript([msg(Role.ASSISTANT, "", { toolCalls: [{ id: "c1", name: "bash" }] })]);
    expect(t!.entries[0]).toMatchObject({ kind: "call", input: undefined });
  });
});

const dump = (turns: Turn[]) => JSON.stringify(turns, (_k, v) => (typeof v === "bigint" ? `${v}n` : v));

const at = (submission: bigint, e: Event): Event => {
  e.submission = submission;
  return e;
};

function realisticEvents(): Event[] {
  const call = { id: "c1", name: "bash", input: bytes('{"command":"ls"}') };
  const call2 = { id: "c2", name: "read", input: bytes('{"path":"a"}') };
  return [
    sub(SubmissionStatus.QUEUED, { requestId: "r1" }),
    msg(Role.USER, "list files"),
    sub(SubmissionStatus.RUNNING),
    msg(Role.ASSISTANT, "sure", { toolCalls: [call, call2] }),
    ev({ kind: EventKind.APPROVAL_REQUESTED, call, approvalSeq: 7n, approval: ApprovalStatus.PENDING }),
    ev({ kind: EventKind.APPROVAL_DECIDED, approvalSeq: 7n, approval: ApprovalStatus.ALLOWED, decidedBy: "web" }),
    ev({ kind: EventKind.CALL_STARTED, call }),
    msg(Role.TOOL, "a.txt", { toolCallId: "c1" }),
    ev({ kind: EventKind.CALL_DONE, call: { id: "c1", name: "bash" } }),
    ev({ kind: EventKind.CALL_STARTED, call: call2 }),
    ev({ kind: EventKind.COMPACTED }),
    sub(SubmissionStatus.DONE),
    at(2n, sub(SubmissionStatus.QUEUED, { requestId: "r2" })),
    at(2n, msg(Role.USER, "again")),
    at(2n, ev({ kind: EventKind.CALL_REPLAYED, call: { id: "c9", name: "bash" } })),
    at(2n, ev({ kind: EventKind.COMPACTION_FAILED, error: "too big" })),
    at(2n, sub(SubmissionStatus.FAILED, { error: "boom" })),
    msg(Role.TOOL, "late", { toolCallId: "c2", isError: true }),
    ev({ kind: EventKind.CALL_DONE, call: { id: "c2", name: "read" }, isError: true }),
  ];
}

describe("TranscriptBuilder", () => {
  test("incremental updates equal a full build at every split point", () => {
    const events = realisticEvents();
    const want = dump(buildTranscript(events));
    for (let split = 0; split <= events.length; split++) {
      const b = new TranscriptBuilder();
      b.update(events.slice(0, split));
      expect(dump(b.update(events))).toBe(want);
      const stepped = new TranscriptBuilder();
      for (let i = 1; i <= events.length; i += 1 + (split % 3)) stepped.update(events.slice(0, i));
      expect(dump(stepped.update(events))).toBe(want);
    }
  });

  test("an update touching the last turn keeps earlier turns and calls", () => {
    const events = realisticEvents();
    const b = new TranscriptBuilder();
    const before = b.update(events.slice(0, 16));
    const after = b.update(events.slice(0, 17));
    expect(after).not.toBe(before);
    expect(after[0]).toBe(before[0]!);
    for (const [i, e] of before[0]!.entries.entries()) expect(after[0]!.entries[i]).toBe(e);
    expect(after[1]).not.toBe(before[1]!);
  });

  test("an update to one call replaces only that call and its turn", () => {
    const events = realisticEvents();
    const b = new TranscriptBuilder();
    const before = b.update(events.slice(0, 8));
    const after = b.update(events.slice(0, 9));
    const [, , a2, a3] = after[0]!.entries;
    const [, , b2, b3] = before[0]!.entries;
    expect(a2).not.toBe(b2!);
    expect(a3).toBe(b3!);
  });

  test("an unchanged list returns the same array", () => {
    const events = realisticEvents();
    const b = new TranscriptBuilder();
    const first = b.update(events);
    expect(b.update(events)).toBe(first);
    expect(b.update(events.slice())).toBe(first);
  });

  test("a shorter or different list rebuilds", () => {
    const events = realisticEvents();
    const b = new TranscriptBuilder();
    b.update(events);
    expect(dump(b.update(events.slice(0, 5)))).toBe(dump(buildTranscript(events.slice(0, 5))));
    expect(b.update([])).toEqual([]);
    b.update(events);
    const other = events.slice(3);
    expect(dump(b.update(other))).toBe(dump(buildTranscript(other)));
  });

  test("thousands of events build incrementally", () => {
    const events: Event[] = [];
    for (let t = 1n; t <= 500n; t++) {
      const id = `call-${t}`;
      const call = { id, name: "bash", input: bytes('{"command":"ls"}') };
      events.push(
        at(t, sub(SubmissionStatus.QUEUED, { requestId: `r${t}` })),
        at(t, msg(Role.USER, `q${t}`)),
        at(t, msg(Role.ASSISTANT, "ok", { toolCalls: [call] })),
        at(t, ev({ kind: EventKind.CALL_STARTED, call })),
        at(t, msg(Role.TOOL, "out", { toolCallId: id })),
        at(t, ev({ kind: EventKind.CALL_DONE, call: { id, name: "bash" } })),
        at(t, msg(Role.ASSISTANT, "done")),
        at(t, sub(SubmissionStatus.RUNNING)),
        at(t, ev({ kind: EventKind.COMPACTED })),
        at(t, sub(SubmissionStatus.DONE)),
      );
    }
    expect(events).toHaveLength(5000);
    const b = new TranscriptBuilder();
    let turns: Turn[] = [];
    for (let i = 10; i <= events.length; i += 10) turns = b.update(events.slice(0, i));
    expect(turns).toHaveLength(500);
    expect(dump(turns)).toBe(dump(buildTranscript(events)));
  });
});
