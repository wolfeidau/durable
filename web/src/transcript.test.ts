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
import { buildTranscript } from "./transcript";

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
