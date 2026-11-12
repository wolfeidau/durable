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

interface CallRef {
  seq: bigint;
  idx: number;
  call: CallEntry;
}

// Applies events copy-on-write, so a turn or call that no event touched keeps its identity for memoised views.
export class TranscriptBuilder {
  private processed = 0;
  private first: Event | undefined;
  private last: Event | undefined;
  private result: Turn[] = [];
  private index = new Map<bigint, number>();
  private calls = new Map<string, CallRef>();
  private approvals = new Map<bigint, CallRef>();
  private freshTurns = new Set<bigint>();
  private freshCalls = new Set<string>();
  private copied = false;

  update(events: readonly Event[]): Turn[] {
    const extends_ =
      this.processed > 0 &&
      events.length >= this.processed &&
      events[0] === this.first &&
      events[this.processed - 1] === this.last;
    if (!extends_) {
      if (this.processed === 0 && events.length === 0) return this.result;
      this.reset();
    }
    if (events.length === this.processed) return this.result;
    this.freshTurns.clear();
    this.freshCalls.clear();
    this.copied = false;
    for (let i = this.processed; i < events.length; i++) this.apply(events[i]!);
    this.processed = events.length;
    this.first = events[0];
    this.last = events[events.length - 1];
    return this.result;
  }

  private reset(): void {
    this.processed = 0;
    this.first = undefined;
    this.last = undefined;
    this.result = [];
    this.index.clear();
    this.calls.clear();
    this.approvals.clear();
  }

  private writable(): Turn[] {
    if (!this.copied) {
      this.result = [...this.result];
      this.copied = true;
    }
    return this.result;
  }

  private ensure(seq: bigint): void {
    if (this.index.has(seq)) return;
    const t: Turn = { seq, status: SubmissionStatus.UNSPECIFIED, entries: [] };
    const r = this.writable();
    this.index.set(seq, r.length);
    r.push(t);
    this.freshTurns.add(seq);
  }

  private edit(seq: bigint): Turn {
    this.ensure(seq);
    const r = this.writable();
    const at = this.index.get(seq)!;
    if (this.freshTurns.has(seq)) return r[at]!;
    const t = { ...r[at]!, entries: [...r[at]!.entries] };
    r[at] = t;
    this.freshTurns.add(seq);
    return t;
  }

  private editCall(id: string): CallEntry {
    const ref = this.calls.get(id)!;
    if (this.freshCalls.has(id)) return ref.call;
    ref.call = { ...ref.call };
    this.edit(ref.seq).entries[ref.idx] = ref.call;
    this.freshCalls.add(id);
    return ref.call;
  }

  private callFor(seq: bigint, id: string, name: string, input?: string): CallRef {
    let ref = this.calls.get(id);
    if (!ref) {
      const turn = this.edit(seq);
      ref = { seq, idx: turn.entries.length, call: { kind: "call", id, name, input, rewritten: false, isError: false } };
      turn.entries.push(ref.call);
      this.calls.set(id, ref);
      this.freshCalls.add(id);
    }
    return ref;
  }

  private apply(e: Event): void {
    const seq = e.submission;
    this.ensure(seq);
    switch (e.kind) {
      case EventKind.SUBMISSION: {
        const t = this.edit(seq);
        t.status = e.status;
        if (e.requestId) t.requestId = e.requestId;
        if (e.error) t.error = e.error;
        break;
      }
      case EventKind.MESSAGE: {
        const m = e.message;
        if (!m) break;
        if (m.role === Role.USER) {
          this.edit(seq).entries.push({ kind: m.note ? "notice" : "user", text: m.text });
        } else if (m.role === Role.ASSISTANT) {
          if (m.text) this.edit(seq).entries.push({ kind: "assistant", text: m.text });
          for (const tc of m.toolCalls) this.callFor(seq, tc.id, tc.name, decode(tc.input));
        } else if (m.role === Role.TOOL) {
          const id = m.toolCallId;
          this.callFor(seq, id, "tool");
          const c = this.editCall(id);
          c.result = m.text;
          c.isError = m.isError;
        }
        break;
      }
      case EventKind.APPROVAL_REQUESTED: {
        if (!e.call) break;
        const ref = this.callFor(seq, e.call.id, e.call.name, decode(e.call.input));
        const c = this.editCall(e.call.id);
        c.approval = { seq: e.approvalSeq, status: ApprovalStatus.PENDING, decidedBy: "" };
        this.approvals.set(e.approvalSeq, ref);
        if (e.rewritten) {
          c.rewritten = true;
          c.input = undefined;
        }
        break;
      }
      case EventKind.APPROVAL_DECIDED: {
        const ref = this.approvals.get(e.approvalSeq) ?? (e.call ? this.calls.get(e.call.id) : undefined);
        if (ref) this.editCall(ref.call.id).approval = { seq: e.approvalSeq, status: e.approval, decidedBy: e.decidedBy };
        break;
      }
      case EventKind.CALL_STARTED:
        if (e.call) {
          const ref = this.callFor(seq, e.call.id, e.call.name, decode(e.call.input));
          if (ref.call.input === undefined && !ref.call.rewritten) this.editCall(e.call.id).input = decode(e.call.input);
        }
        break;
      case EventKind.CALL_REPLAYED:
        this.edit(seq).entries.push({ kind: "notice", text: `${e.call?.name ?? "A tool call"} ran again after a restart` });
        break;
      case EventKind.CALL_DONE:
        if (e.call) {
          this.callFor(seq, e.call.id, e.call.name);
          this.editCall(e.call.id).isError = e.isError;
        }
        break;
      case EventKind.COMPACTED:
        this.edit(seq).entries.push({ kind: "notice", text: "Earlier messages were compacted" });
        break;
      case EventKind.COMPACTION_FAILED:
        this.edit(seq).entries.push({
          kind: "notice",
          text: `Compaction failed${e.error ? `: ${e.error}` : ""}`,
        });
        break;
    }
  }
}

export function buildTranscript(events: readonly Event[]): Turn[] {
  return new TranscriptBuilder().update(events);
}
