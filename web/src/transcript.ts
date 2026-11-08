import { ApprovalStatus, EventKind, Role, SubmissionStatus, type Event } from "./gen/durable/v1/durable_pb";

export interface ApprovalEntry {
  seq: bigint;
  status: ApprovalStatus;
  decidedBy: string;
}

export interface CallEntry {
  kind: "call";
  id: string;
  name: string;
  input?: string;
  rewritten: boolean;
  result?: string;
  isError: boolean;
  approval?: ApprovalEntry;
}

export type Entry = { kind: "user" | "assistant" | "notice"; text: string } | CallEntry;

export interface Turn {
  seq: bigint;
  requestId?: string;
  status: SubmissionStatus;
  error?: string;
  entries: Entry[];
}

const decoder = new TextDecoder();
const decode = (b: Uint8Array | undefined): string | undefined => (b && b.length ? decoder.decode(b) : undefined);

export function buildTranscript(events: readonly Event[]): Turn[] {
  const turns = new Map<bigint, Turn>();
  const calls = new Map<string, CallEntry>();
  const approvals = new Map<bigint, CallEntry>();

  const turnFor = (seq: bigint): Turn => {
    let t = turns.get(seq);
    if (!t) {
      t = { seq, status: SubmissionStatus.UNSPECIFIED, entries: [] };
      turns.set(seq, t);
    }
    return t;
  };
  const callFor = (turn: Turn, id: string, name: string, input?: string): CallEntry => {
    let c = calls.get(id);
    if (!c) {
      c = { kind: "call", id, name, input, rewritten: false, isError: false };
      calls.set(id, c);
      turn.entries.push(c);
    }
    return c;
  };

  for (const e of events) {
    const turn = turnFor(e.submission);
    switch (e.kind) {
      case EventKind.SUBMISSION:
        turn.status = e.status;
        if (e.requestId) turn.requestId = e.requestId;
        if (e.error) turn.error = e.error;
        break;
      case EventKind.MESSAGE: {
        const m = e.message;
        if (!m) break;
        if (m.role === Role.USER) {
          turn.entries.push({ kind: "user", text: m.text });
        } else if (m.role === Role.ASSISTANT) {
          if (m.text) turn.entries.push({ kind: "assistant", text: m.text });
          for (const tc of m.toolCalls) callFor(turn, tc.id, tc.name, decode(tc.input));
        } else if (m.role === Role.TOOL) {
          const c = callFor(turn, m.toolCallId, "tool");
          c.result = m.text;
          c.isError = m.isError;
        }
        break;
      }
      case EventKind.APPROVAL_REQUESTED: {
        if (!e.call) break;
        const c = callFor(turn, e.call.id, e.call.name, decode(e.call.input));
        c.approval = { seq: e.approvalSeq, status: ApprovalStatus.PENDING, decidedBy: "" };
        approvals.set(e.approvalSeq, c);
        if (e.rewritten) {
          c.rewritten = true;
          c.input = undefined;
        }
        break;
      }
      case EventKind.APPROVAL_DECIDED: {
        const c = approvals.get(e.approvalSeq) ?? (e.call ? calls.get(e.call.id) : undefined);
        if (c) c.approval = { seq: e.approvalSeq, status: e.approval, decidedBy: e.decidedBy };
        break;
      }
      case EventKind.CALL_STARTED:
        if (e.call) {
          const c = callFor(turn, e.call.id, e.call.name, decode(e.call.input));
          if (c.input === undefined && !c.rewritten) c.input = decode(e.call.input);
        }
        break;
      case EventKind.CALL_REPLAYED:
        turn.entries.push({ kind: "notice", text: `${e.call?.name ?? "A tool call"} ran again after a restart` });
        break;
      case EventKind.CALL_DONE:
        if (e.call) callFor(turn, e.call.id, e.call.name).isError = e.isError;
        break;
      case EventKind.COMPACTED:
        turn.entries.push({ kind: "notice", text: "Earlier messages were compacted" });
        break;
      case EventKind.COMPACTION_FAILED:
        turn.entries.push({ kind: "notice", text: `Compaction failed${e.error ? `: ${e.error}` : ""}` });
        break;
    }
  }
  return [...turns.values()];
}
