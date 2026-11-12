import { ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useTransport } from "@connectrpc/connect-query";
import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactElement } from "react";
import { cancel, decideApproval, listSessions, submit } from "./gen/durable/v1/durable-SessionService_connectquery";
import { ApprovalStatus, SubmissionStatus } from "./gen/durable/v1/durable_pb";
import { kindLabel, where } from "./sessions";
import type { WatchSummary } from "./StatusLine";
import { buildTranscript, type CallEntry, type Turn } from "./transcript";
import { getToken } from "./token";
import { useWatchEvents } from "./useWatchEvents";

const errText = (err: unknown) => ConnectError.from(err).rawMessage;

const statusText: Record<number, string> = {
  [SubmissionStatus.QUEUED]: "queued",
  [SubmissionStatus.RUNNING]: "running",
  [SubmissionStatus.DONE]: "done",
  [SubmissionStatus.FAILED]: "failed",
  [SubmissionStatus.CANCELLED]: "cancelled",
};

const statusTone: Record<number, string> = {
  [SubmissionStatus.QUEUED]: "link",
  [SubmissionStatus.RUNNING]: "link pulse",
  [SubmissionStatus.DONE]: "ok",
  [SubmissionStatus.FAILED]: "error",
  [SubmissionStatus.CANCELLED]: "cancelled",
};

const decisionText: Record<number, string> = {
  [ApprovalStatus.ALLOWED]: "allowed",
  [ApprovalStatus.DECLINED]: "denied",
  [ApprovalStatus.CANCELLED]: "cancelled",
};

function pretty(input: string): string {
  try {
    return JSON.stringify(JSON.parse(input), null, 2);
  } catch {
    return input;
  }
}

const active = (s: SubmissionStatus) => s === SubmissionStatus.QUEUED || s === SubmissionStatus.RUNNING;

interface CallProps {
  call: CallEntry;
  output?: string;
  onDecide: (seq: bigint, allow: boolean) => void;
  busy: boolean;
}

function chip(call: CallEntry, output: string | undefined): { text: string; tone: string } {
  const a = call.approval;
  if (a?.status === ApprovalStatus.PENDING) return { text: "waiting", tone: "warn" };
  if (a?.status === ApprovalStatus.DECLINED) return { text: "denied", tone: "error" };
  if (a?.status === ApprovalStatus.CANCELLED) return { text: "cancelled", tone: "cancelled" };
  if (call.result !== undefined) return call.isError ? { text: "error", tone: "error" } : { text: "done", tone: "ok" };
  return { text: "running", tone: output ? "link pulse" : "link" };
}

function Call({ call, output, onDecide, busy }: CallProps): ReactElement {
  const a = call.approval;
  const pending = a?.status === ApprovalStatus.PENDING;
  const state = chip(call, output);
  return (
    <div className={`call${pending ? " call-pending" : ""}`}>
      <div className="call-head">
        <span className="call-name">{call.name}</span>
        <span className={`badge ${state.tone}`}>{state.text}</span>
      </div>
      <div className="call-body">
        {call.rewritten ? (
          <p className="muted">input changed by a hook, hidden</p>
        ) : (
          call.input !== undefined && <pre className="console">{pretty(call.input)}</pre>
        )}
        {a && !pending && decisionText[a.status] && (
          <p className="muted">
            {decisionText[a.status]}
            {a.decidedBy && ` by ${a.decidedBy}`}
          </p>
        )}
        {call.result !== undefined ? (
          <pre className={`console result${call.isError ? " error" : ""}`}>{call.result}</pre>
        ) : (
          output && <pre className="console result live">{output}</pre>
        )}
      </div>
      {pending && (
        <div className="approval banner warn">
          <span>Waiting for approval</span>
          <button disabled={busy} onClick={() => onDecide(a.seq, true)}>
            Allow
          </button>
          <button className="secondary" disabled={busy} onClick={() => onDecide(a.seq, false)}>
            Deny
          </button>
        </div>
      )}
    </div>
  );
}

interface TurnProps {
  turn: Turn;
  localText?: string;
  partial?: string;
  toolOutput: ReadonlyMap<string, string>;
  onDecide: (seq: bigint, allow: boolean) => void;
  onCancel?: () => void;
  busy: boolean;
}

function TurnView({ turn, localText, partial, toolOutput, onDecide, onCancel, busy }: TurnProps): ReactElement {
  const hasUser = turn.entries.some((e) => e.kind === "user");
  return (
    <section className="turn">
      {!hasUser && localText && <div className="msg user">{localText}</div>}
      {turn.entries.map((e, i) => {
        if (e.kind === "call") {
          return <Call key={i} call={e} output={toolOutput.get(e.id)} onDecide={onDecide} busy={busy} />;
        }
        return (
          <div key={i} className={e.kind === "notice" ? "notice" : `msg ${e.kind}`}>
            {e.text}
          </div>
        );
      })}
      {partial && <div className="msg assistant live">{partial}</div>}
      <div className="turn-foot">
        {statusText[turn.status] && (
          <span className={`badge ${statusTone[turn.status]}`}>{statusText[turn.status]}</span>
        )}
        {turn.error && <span className="error-text">{turn.error}</span>}
        {onCancel && active(turn.status) && (
          <button className="secondary" onClick={onCancel}>
            Cancel
          </button>
        )}
      </div>
    </section>
  );
}

interface Props {
  sessionId: string;
  onWatch: (w: WatchSummary | undefined) => void;
}

export function SessionView({ sessionId, onWatch }: Props): ReactElement {
  const transport = useTransport();
  const w = useWatchEvents(transport, sessionId, getToken);
  const resume = useRef(w.resumeAuth);
  resume.current = w.resumeAuth;
  const { status, lastError } = w;
  useEffect(() => {
    onWatch({ status, lastError, resumeAuth: () => resume.current() });
  }, [status, lastError, onWatch]);
  useEffect(() => () => onWatch(undefined), [onWatch]);
  const sessions = useQuery(listSessions, {});
  const meta = sessions.data?.sessions.find((s) => s.id === sessionId);
  const turns = useMemo(() => buildTranscript(w.events), [w.events]);
  const [text, setText] = useState("");
  const [note, setNote] = useState("");
  // This tab's own submissions, so a queued turn has text before the user message commits.
  const [mine, setMine] = useState<ReadonlyMap<string, string>>(new Map());

  const submitM = useMutation(submit);
  const cancelM = useMutation(cancel);
  const decideM = useMutation(decideApproval);

  const send = () => {
    const prompt = text.trim();
    if (!prompt) return;
    const requestId = crypto.randomUUID();
    setMine((m) => new Map(m).set(requestId, prompt));
    setText("");
    setNote("");
    submitM.mutate(
      { sessionId, requestId, text: prompt },
      {
        onError: (err) => {
          setNote(errText(err));
          setText(prompt);
          setMine((m) => {
            const next = new Map(m);
            next.delete(requestId);
            return next;
          });
        },
      },
    );
  };

  const stop = (requestId: string) =>
    cancelM.mutate({ sessionId, requestId }, { onError: (err) => setNote(errText(err)) });

  const decide = (approvalSeq: bigint, allow: boolean) =>
    decideM.mutate({ sessionId, approvalSeq, allow, by: "web" }, { onError: (err) => setNote(errText(err)) });

  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      send();
    }
  };

  const seen = new Set(turns.map((t) => t.requestId));
  const unseen = [...mine].filter(([id]) => !seen.has(id));

  return (
    <div className="session-view">
      <div className="info card">
        <h1>{meta?.name || sessionId}</h1>
        {meta && (
          <p className="info-where">
            <span className="kind faint">{kindLabel(meta)}</span>
            <span className="mono" title={where(meta)}>
              {where(meta)}
            </span>
          </p>
        )}
        <p className="session-id mono faint" title={sessionId}>
          {sessionId}
        </p>
      </div>
      <div className="transcript">
        {turns.map((t) => (
          <TurnView
            key={String(t.seq)}
            turn={t}
            localText={t.requestId ? mine.get(t.requestId) : undefined}
            partial={w.partialText.get(t.seq)}
            toolOutput={w.toolOutput}
            onDecide={decide}
            onCancel={t.requestId ? () => stop(t.requestId!) : undefined}
            busy={decideM.isPending}
          />
        ))}
        {unseen.map(([id, prompt]) => (
          <section className="turn" key={id}>
            <div className="msg user">{prompt}</div>
            <div className="turn-foot">
              <span className="muted">sending</span>
              <button className="secondary" onClick={() => stop(id)}>
                Cancel
              </button>
            </div>
          </section>
        ))}
        {turns.length === 0 && unseen.length === 0 && <p className="muted">No messages yet.</p>}
      </div>
      {note && <p className="banner error">{note}</p>}
      <div className="composer">
        <textarea
          value={text}
          rows={3}
          placeholder="Message"
          aria-label="Message"
          onChange={(e) => setText(e.target.value)}
          onKeyDown={onKey}
        />
        <div className="composer-foot">
          <span className="faint hint">⌘/Ctrl+Enter to send</span>
          <button disabled={!text.trim()} onClick={send}>
            Submit
          </button>
        </div>
      </div>
    </div>
  );
}
