import { ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  createContext,
  memo,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactElement,
} from "react";
import { cancel, decideApproval, listSessions, submit } from "./gen/durable/v1/durable-SessionService_connectquery";
import { ApprovalStatus, SubmissionStatus } from "./gen/durable/v1/durable_pb";
import { activeTurn, activityText, isActive } from "./activity";
import { chip, defaultOpen, isRunning, summary } from "./calls";
import { CopyButton } from "./CopyButton";
import { Markdown } from "./markdown";
import type { WatchSummary } from "./StatusLine";
import { ReportPanel } from "./ReportPanel";
import { buildOverview } from "./report";
import { TranscriptBuilder, type CallEntry, type Turn } from "./transcript";
import { nearBottom } from "./scroll";
import { getToken } from "./token";
import { buildReport, compact, money, percent, summarize, turnFooter } from "./usage";
import { useNow } from "./useNow";
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

const OpenCalls = createContext<{ open: boolean; version: number }>({ open: false, version: 0 });

// The element id a link into the transcript is aiming at, until it has been scrolled to.
const FocusTarget = createContext<string | undefined>(undefined);

const noOutput: ReadonlyMap<string, string> = new Map();

interface CallProps {
  call: CallEntry;
  output?: string;
  onDecide: (seq: bigint, allow: boolean) => void;
  busy: boolean;
}

// Only a running call ticks, so finished calls never re-render on the clock.
function CallBadge({ call, output }: { call: CallEntry; output?: string }): ReactElement {
  const now = useNow(call.startedAt !== undefined && isRunning(call));
  const state = chip(call, output, now);
  return <span className={`badge ${state.tone}`}>{state.text}</span>;
}

const Call = memo(function Call({ call, output, onDecide, busy }: CallProps): ReactElement {
  const a = call.approval;
  const pending = a?.status === ApprovalStatus.PENDING;
  const [open, setOpen] = useState(() => defaultOpen(call, output));
  const touched = useRef(false);
  const bulk = useContext(OpenCalls);
  const seenVersion = useRef(bulk.version);
  const wanted = defaultOpen(call, output);
  const focus = useContext(FocusTarget);
  const domId = `call-${call.id}`;

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
  useEffect(() => {
    if (focus !== domId) return;
    touched.current = true;
    setOpen(true);
  }, [focus, domId]);

  const preview = summary(call);
  return (
    <div id={domId} className={`call${pending ? " call-pending" : ""}`}>
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
          <CallBadge call={call} output={output} />
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
  usage?: string;
  localText?: string;
  partial?: string;
  toolOutput: ReadonlyMap<string, string>;
  onDecide: (seq: bigint, allow: boolean) => void;
  onCancel: (requestId: string) => void;
  busy: boolean;
}

const TurnView = memo(function TurnView({ turn, usage, localText, partial, toolOutput, onDecide, onCancel, busy }: TurnProps): ReactElement {
  const hasUser = turn.entries.some((e) => e.kind === "user");
  const { requestId } = turn;
  return (
    <section className="turn" id={`turn-${turn.seq}`}>
      {!hasUser && localText && <div className="msg user">{localText}</div>}
      {turn.entries.map((e, i) => {
        if (e.kind === "call") {
          return <Call key={i} call={e} output={toolOutput.get(e.id)} onDecide={onDecide} busy={busy} />;
        }
        if (e.kind === "assistant") {
          return (
            <div key={i} className="msg assistant">
              <Markdown text={e.text} />
              <CopyButton text={e.text} />
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
        {usage && !isActive(turn.status) && <span className="turn-usage muted">{usage}</span>}
        {requestId && isActive(turn.status) && (
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

function Activity({ turn }: { turn: Turn }): ReactElement {
  const now = useNow(true);
  return <span className="tone-link activity">{activityText(turn, now)}</span>;
}

interface Props {
  sessionId: string;
  onWatch: (w: WatchSummary | undefined) => void;
  tab?: "report";
  onTab: (tab: "report" | undefined) => void;
  focus?: string;
  onFocus: (target: string | undefined) => void;
}

export function SessionView({ sessionId, onWatch, tab, onTab, focus, onFocus }: Props): ReactElement {
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
  const stats = useMemo(() => summarize(w.events), [w.events]);
  const report = useMemo(() => buildReport(w.events), [w.events]);
  const overview = useMemo(() => buildOverview(w.events, turns, report), [w.events, turns, report]);
  const footers = useMemo(() => new Map(report.turns.map((t) => [t.seq, turnFooter(t)])), [report]);
  const [calls, setCalls] = useState({ open: false, version: 0 });
  const [text, setText] = useState("");
  const [behind, setBehind] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const composer = useRef<HTMLDivElement>(null);
  const transcript = useRef<HTMLDivElement>(null);
  const following = useRef(true);
  const [note, setNote] = useState("");
  // This tab's own submissions, so a queued turn has text before the user message commits.
  const [mine, setMine] = useState<ReadonlyMap<string, string>>(new Map());

  const submitM = useMutation(submit);
  const cancelM = useMutation(cancel);
  const decideM = useMutation(decideApproval);

  const send = () => {
    const prompt = text.trim();
    if (!prompt) return;
    following.current = true;
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
  const loading = w.events.length === 0 && (status === "connecting" || status === "reconnecting");
  const latest = activeTurn(turns);
  const onReport = tab === "report" && !focus;

  const toBottom = () => window.scrollTo({ top: document.documentElement.scrollHeight });

  // The user's own scrolling decides whether new content is followed, so scrolling up is never fought.
  useEffect(() => {
    const onScroll = () => {
      const el = document.documentElement;
      following.current = nearBottom(el.scrollTop, el.clientHeight, el.scrollHeight);
      if (following.current) setBehind(false);
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, []);

  // Not following keeps the arriving content from pulling the view away from the target.
  const jump = useCallback(
    (target: string) => {
      following.current = false;
      onFocus(target);
    },
    [onFocus],
  );

  // The URL drops the target once applied, so a reload doesn't jump again.
  useEffect(() => {
    if (!focus || onReport) return;
    const el = document.getElementById(focus);
    if (!el) return;
    following.current = false;
    const reveal = () => el.scrollIntoView({ block: "start" });
    reveal();
    // Turns off screen are laid out lazily, so the first scroll can land short.
    requestAnimationFrame(() => requestAnimationFrame(reveal));
    el.classList.remove("flash");
    void el.offsetWidth;
    el.classList.add("flash");
    setTimeout(() => el.classList.remove("flash"), 1500);
    onFocus(undefined);
  }, [focus, onReport, turns, onFocus]);

  useLayoutEffect(() => {
    if (onReport) return;
    if (following.current) toBottom();
    else setBehind(true);
  }, [w.events, w.partialText, w.toolOutput, mine, loading, onReport]);

  // The transcript's bottom padding tracks the composer, and content that grows after render (calls settling, markdown) is still followed.
  useLayoutEffect(() => {
    const el = composer.current;
    const host = root.current;
    if (!el || !host) return;
    const measure = () => {
      host.style.setProperty("--composer-h", `${el.offsetHeight}px`);
      if (following.current) toBottom();
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    if (transcript.current) ro.observe(transcript.current);
    return () => {
      ro.disconnect();
      host.style.removeProperty("--composer-h");
    };
  }, [onReport]);

  return (
    <div className="session-view" ref={root}>
      <div className="toolbar">
        <div className="tabs" role="group" aria-label="View">
          <button className="secondary" aria-pressed={!onReport} onClick={() => onTab(undefined)}>
            Transcript
          </button>
          <button className="secondary" aria-pressed={onReport} onClick={() => onTab("report")}>
            Report
          </button>
        </div>
        {!onReport && (
          <div className="tool-actions">
            <button
              className="secondary"
              aria-label="Collapse all tool calls"
              title="Collapse all tool calls"
              onClick={() => setCalls((c) => ({ open: false, version: c.version + 1 }))}
            >
              Collapse all
            </button>
            <button
              className="secondary"
              aria-label="Expand all tool calls"
              title="Expand all tool calls"
              onClick={() => setCalls((c) => ({ open: true, version: c.version + 1 }))}
            >
              Expand all
            </button>
          </div>
        )}
      </div>
      {onReport && <ReportPanel report={report} overview={overview} stats={stats} onJump={jump} />}
      <FocusTarget.Provider value={focus}>
      <OpenCalls.Provider value={calls}>
        <div className="transcript" ref={transcript} hidden={onReport}>
          {turns.map((t) => (
            <TurnView
              key={String(t.seq)}
              turn={t}
              usage={footers.get(t.seq)}
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
          {loading && <p className="muted">Loading…</p>}
          {!loading && turns.length === 0 && unseen.length === 0 && <p className="muted">No messages yet.</p>}
        </div>
      </OpenCalls.Provider>
      </FocusTarget.Provider>
      {!onReport && (
        <>
          {behind && (
            <button
              className="latest"
              onClick={() => {
                following.current = true;
                setBehind(false);
                toBottom();
              }}
            >
              ↓ Latest
            </button>
          )}
          <div className="composer" ref={composer}>
            {note && <p className="banner tone-error">{note}</p>}
            <textarea
              value={text}
              rows={3}
              placeholder="Message"
              aria-label="Message"
              onChange={(e) => setText(e.target.value)}
              onKeyDown={onKey}
            />
            <div className="composer-foot">
              <div className="stats">
                <div className="meter">
                  <span className="stat-label" id="ctx-label">
                    Context
                  </span>
                  {stats.contextFraction !== undefined ? (
                    <>
                      <meter
                        aria-labelledby="ctx-label"
                        min={0}
                        max={1}
                        low={0.7}
                        high={0.9}
                        optimum={0}
                        value={Math.min(1, stats.contextFraction)}
                        aria-valuetext={`${percent(stats.contextFraction)} of ${compact(stats.contextWindow!)}`}
                      />
                      <span>
                        {percent(stats.contextFraction)} of {compact(stats.contextWindow!)}
                      </span>
                    </>
                  ) : (
                    <span>{stats.contextTokens ? `${compact(stats.contextTokens)} tokens` : "—"}</span>
                  )}
                </div>
                <span title={stats.unknownCost ? `${stats.unknownCost} replies have no known cost` : undefined}>
                  <span className="stat-label">Cost </span>
                  {money(stats.cost, stats.estimated)}
                </span>
                <span>
                  <span className="stat-label">Cache hit </span>
                  {percent(stats.cacheHit)}
                </span>
                {latest && <Activity turn={latest} />}
              </div>
              <div className="send">
                <span className="faint hint">⌘/Ctrl+Enter</span>
                <button disabled={!text.trim()} onClick={send}>
                  Submit
                </button>
              </div>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
