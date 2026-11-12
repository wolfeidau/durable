import { ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  createContext,
  memo,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactElement,
} from "react";
import { cancel, decideApproval, listSessions, submit } from "./gen/durable/v1/durable-SessionService_connectquery";
import { ApprovalStatus, SubmissionStatus } from "./gen/durable/v1/durable_pb";
import { chip, defaultOpen, summary } from "./calls";
import { Markdown } from "./markdown";
import { kindLabel, where } from "./sessions";
import type { WatchSummary } from "./StatusLine";
import { TranscriptBuilder, type CallEntry, type Turn } from "./transcript";
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
  [SubmissionStatus.QUEUED]: "tone-link",
  [SubmissionStatus.RUNNING]: "tone-link pulse",
  [SubmissionStatus.DONE]: "tone-ok",
  [SubmissionStatus.FAILED]: "tone-error",
  [SubmissionStatus.CANCELLED]: "tone-cancelled",
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

const OpenCalls = createContext<{ open: boolean; version: number }>({ open: false, version: 0 });

const noOutput: ReadonlyMap<string, string> = new Map();

interface CallProps {
  call: CallEntry;
  output?: string;
  onDecide: (seq: bigint, allow: boolean) => void;
  busy: boolean;
}

const Call = memo(function Call({ call, output, onDecide, busy }: CallProps): ReactElement {
  const a = call.approval;
  const pending = a?.status === ApprovalStatus.PENDING;
  const state = chip(call, output);
  const [open, setOpen] = useState(() => defaultOpen(call, output));
  const touched = useRef(false);
  const bulk = useContext(OpenCalls);
  const seenVersion = useRef(bulk.version);
  const wanted = defaultOpen(call, output);

  // Until the user toggles, the default decides, so a call that loads mid-page doesn't stay open after it finishes.
  useEffect(() => {
    if (!touched.current) setOpen(wanted);
  }, [wanted]);
  useEffect(() => {
    if (bulk.version === seenVersion.current) return;
    seenVersion.current = bulk.version;
    touched.current = true;
    setOpen(bulk.open);
  }, [bulk]);

  const preview = summary(call);
  return (
    <div className={`call${pending ? " call-pending" : ""}`}>
      <details
        open={open}
        onToggle={(e) => {
          // A change that already matches state came from React, not the user.
          if (e.currentTarget.open === open) return;
          touched.current = true;
          setOpen(e.currentTarget.open);
        }}
      >
        <summary className="call-head">
          <span className="call-name">{call.name}</span>
          <span className="call-summary">{preview}</span>
          <span className={`badge ${state.tone}`}>{state.text}</span>
        </summary>
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
      </details>
      {pending && (
        <div className="approval banner tone-warn">
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
});

interface TurnProps {
  turn: Turn;
  localText?: string;
  partial?: string;
  toolOutput: ReadonlyMap<string, string>;
  onDecide: (seq: bigint, allow: boolean) => void;
  onCancel: (requestId: string) => void;
  busy: boolean;
}

const TurnView = memo(function TurnView({ turn, localText, partial, toolOutput, onDecide, onCancel, busy }: TurnProps): ReactElement {
  const hasUser = turn.entries.some((e) => e.kind === "user");
  const { requestId } = turn;
  return (
    <section className="turn">
      {!hasUser && localText && <div className="msg user">{localText}</div>}
      {turn.entries.map((e, i) => {
        if (e.kind === "call") {
          return <Call key={i} call={e} output={toolOutput.get(e.id)} onDecide={onDecide} busy={busy} />;
        }
        if (e.kind === "assistant") {
          return (
            <div key={i} className="msg assistant">
              <Markdown text={e.text} />
            </div>
          );
        }
        return (
          <div key={i} className={e.kind === "notice" ? "notice" : `msg ${e.kind}`}>
            {e.text}
          </div>
        );
      })}
      {partial && (
        <div className="msg assistant live">
          <Markdown text={partial} />
        </div>
      )}
      <div className="turn-foot">
        {statusText[turn.status] && (
          <span className={`badge ${statusTone[turn.status]}`}>{statusText[turn.status]}</span>
        )}
        {turn.error && <span className="error-text">{turn.error}</span>}
        {requestId && active(turn.status) && (
          <button className="secondary" onClick={() => onCancel(requestId)}>
            Cancel
          </button>
        )}
      </div>
    </section>
  );
});

const PendingTurn = memo(function PendingTurn({ id, prompt, onCancel }: { id: string; prompt: string; onCancel: (requestId: string) => void }): ReactElement {
  return (
    <section className="turn">
      <div className="msg user">{prompt}</div>
      <div className="turn-foot">
        <span className="muted">sending</span>
        <button className="secondary" onClick={() => onCancel(id)}>
          Cancel
        </button>
      </div>
    </section>
  );
});

// Only a turn with a running call reads live output, so finished turns share one empty map and stay memoised.
function liveOutput(turn: Turn, all: ReadonlyMap<string, string>): ReadonlyMap<string, string> {
  let out: Map<string, string> | undefined;
  for (const e of turn.entries) {
    if (e.kind !== "call" || e.result !== undefined) continue;
    const o = all.get(e.id);
    if (o !== undefined) (out ??= new Map()).set(e.id, o);
  }
  return out ?? noOutput;
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
  const builder = useRef(new TranscriptBuilder());
  const turns = useMemo(() => builder.current.update(w.events), [w.events]);
  const [calls, setCalls] = useState({ open: false, version: 0 });
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

  const { mutate: cancelMutate } = cancelM;
  const { mutate: decideMutate } = decideM;
  const stop = useCallback(
    (requestId: string) => cancelMutate({ sessionId, requestId }, { onError: (err) => setNote(errText(err)) }),
    [cancelMutate, sessionId],
  );

  const decide = useCallback(
    (approvalSeq: bigint, allow: boolean) =>
      decideMutate({ sessionId, approvalSeq, allow, by: "web" }, { onError: (err) => setNote(errText(err)) }),
    [decideMutate, sessionId],
  );

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
        <div className="actions">
          <button className="secondary" onClick={() => setCalls((c) => ({ open: false, version: c.version + 1 }))}>
            Collapse tool calls
          </button>
          <button className="secondary" onClick={() => setCalls((c) => ({ open: true, version: c.version + 1 }))}>
            Expand tool calls
          </button>
        </div>
      </div>
      <OpenCalls.Provider value={calls}>
        <div className="transcript">
          {turns.map((t) => (
            <TurnView
              key={String(t.seq)}
              turn={t}
              localText={t.requestId ? mine.get(t.requestId) : undefined}
              partial={w.partialText.get(t.seq)}
              toolOutput={liveOutput(t, w.toolOutput)}
              onDecide={decide}
              onCancel={stop}
              busy={decideM.isPending}
            />
          ))}
          {unseen.map(([id, prompt]) => (
            <PendingTurn key={id} id={id} prompt={prompt} onCancel={stop} />
          ))}
          {turns.length === 0 && unseen.length === 0 && <p className="muted">No messages yet.</p>}
        </div>
      </OpenCalls.Provider>
      {note && <p className="banner tone-error">{note}</p>}
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
