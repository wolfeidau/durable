import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery, useTransport } from "@connectrpc/connect-query";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactElement } from "react";
import { cancel, decideApproval, listSessions, submit } from "../gen/durable/v1/durable-SessionService_connectquery";
import { activeTurn } from "./transcript/activity";
import { Composer } from "./Composer";
import { NetworkPanel, noSandboxText } from "./network/NetworkPanel";
import type { WatchSummary } from "./StatusLine";
import { ReportPanel } from "./report/ReportPanel";
import { buildOverview } from "./report/overview";
import { TranscriptBuilder } from "./transcript/transcript";
import { TranscriptPane } from "./transcript/TranscriptPane";
import type { OpenTab } from "./transcript/Call";
import { errText } from "../lib/rpc";
import { getToken } from "../lib/token";
import { buildReport, summarize, turnFooter } from "./report/usage";
import { useDenied } from "./network/useDenied";
import { useFollowScroll } from "./useFollowScroll";
import { useNetwork } from "./network/useNetwork";
import { useWatchEvents } from "./useWatchEvents";
import { ViewPanel, ViewTabs } from "./ViewTabs";
import { viewOf } from "./view";

interface Props {
  sessionId: string;
  onWatch: (w: WatchSummary | undefined) => void;
  tab?: "report" | "network";
  call?: string;
  onTab: OpenTab;
  focus?: string;
  onFocus: (target: string | undefined) => void;
}

export function SessionView({ sessionId, onWatch, tab, call, onTab, focus, onFocus }: Props): ReactElement {
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
  const view = viewOf(tab, focus);
  const onReport = view === "report";
  const onNetwork = view === "network";
  const onTranscript = !onReport && !onNetwork;
  const sandboxed = Boolean(meta?.environment);
  const net = useNetwork(transport, sessionId, meta?.createdAt ? timestampDate(meta.createdAt) : undefined, sandboxed);
  const { allCalls, deniedCount, denied } = useDenied(net, turns);
  // Only the Report reads it, and it parses every call's input, so it isn't rebuilt per event behind the Transcript.
  const overview = useMemo(() => (onReport ? buildOverview(w.events, turns, report) : undefined), [onReport, w.events, turns, report]);
  const footers = useMemo(() => new Map(report.turns.map((t) => [t.seq, turnFooter(t)])), [report]);
  const [calls, setCalls] = useState({ open: false, version: 0 });
  const [text, setText] = useState("");
  const root = useRef<HTMLDivElement>(null);
  const composer = useRef<HTMLDivElement>(null);
  const transcript = useRef<HTMLDivElement>(null);
  const [note, setNote] = useState("");
  // This tab's own submissions, so a queued turn has text before the user message commits.
  const [mine, setMine] = useState<ReadonlyMap<string, string>>(new Map());

  const submitM = useMutation(submit);
  const cancelM = useMutation(cancel);
  const decideM = useMutation(decideApproval);

  const seen = new Set(turns.map((t) => t.requestId));
  const unseen = [...mine].filter(([id]) => !seen.has(id));
  const loading = w.events.length === 0 && (status === "connecting" || status === "reconnecting");
  const latest = activeTurn(turns);

  const { following, behind, jump, jumpToLatest } = useFollowScroll({
    root,
    composer,
    transcript,
    onTranscript,
    onReport,
    focus,
    onFocus,
    turns,
    events: w.events,
    partialText: w.partialText,
    toolOutput: w.toolOutput,
    mine,
    loading,
  });

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

  return (
    <ViewTabs
      ref={root}
      view={view}
      onTab={onTab}
      sandboxed={sandboxed}
      deniedCount={deniedCount}
      actions={
        onTranscript && (
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
        )
      }
    >
      <ViewPanel id="report">{overview && <ReportPanel report={report} overview={overview} stats={stats} onJump={jump} />}</ViewPanel>
      <ViewPanel id="network">
        {meta && !sandboxed ? (
          <p className="muted">{noSandboxText}</p>
        ) : (
          <NetworkPanel
            state={net}
            calls={allCalls}
            denied={denied}
            call={call}
            onCall={(id) => onTab("network", id)}
            onJump={jump}
          />
        )}
      </ViewPanel>
      <ViewPanel id="transcript">
        <TranscriptPane
          ref={transcript}
          turns={turns}
          footers={footers}
          mine={mine}
          partialText={w.partialText}
          toolOutput={w.toolOutput}
          denied={denied}
          onTab={onTab}
          onDecide={decide}
          onCancel={stop}
          busy={decideM.isPending}
          unseen={unseen}
          loading={loading}
          calls={calls}
          focus={focus}
          hidden={!onTranscript}
        />
      </ViewPanel>
      {onTranscript && (
        <>
          {behind && (
            <button className="latest" onClick={jumpToLatest}>
              ↓ Latest
            </button>
          )}
          <Composer ref={composer} text={text} onText={setText} onSend={send} note={note} stats={stats} latest={latest} />
        </>
      )}
    </ViewTabs>
  );
}
