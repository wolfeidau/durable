import { createContext, memo, useContext, useEffect, useRef, useState, type ReactElement } from "react";
import { ApprovalStatus, type NetworkEvent } from "../../gen/durable/v1/durable_pb";
import { activate } from "../../lib/a11y";
import { plural } from "../../lib/format";
import { tipProps, useTip } from "../../ui/Tooltip";
import { useNow } from "../../ui/useNow";
import { hostSummary } from "../network/networkView";
import { chip, defaultOpen, isRunning, summary } from "./calls";
import type { CallEntry } from "./transcript";

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

export const OpenCalls = createContext<{ open: boolean; version: number }>({ open: false, version: 0 });

// The element id a link into the transcript is aiming at, until it has been scrolled to.
export const FocusTarget = createContext<string | undefined>(undefined);

export type OpenTab = (tab: "report" | "network" | undefined, call?: string) => void;

interface CallProps {
  call: CallEntry;
  output?: string;
  denied?: NetworkEvent[];
  onTab: OpenTab;
  onDecide: (seq: bigint, allow: boolean) => void;
  busy: boolean;
}

// Only a running call ticks, so finished calls never re-render on the clock.
function CallBadge({ call, output }: { call: CallEntry; output?: string }): ReactElement {
  const now = useNow(call.startedAt !== undefined && isRunning(call));
  const state = chip(call, output, now);
  return <span className={`badge ${state.tone}`}>{state.text}</span>;
}

// Sits inside <summary>, so it must stop the click from toggling the call.
function DeniedChip({ id, denied, onTab }: { id: string; denied: NetworkEvent[]; onTab: OpenTab }): ReactElement {
  const tip = useTip();
  const open = (e: { preventDefault: () => void; stopPropagation: () => void }) => {
    e.preventDefault();
    e.stopPropagation();
    onTab("network", id);
  };
  return (
    <span
      role="button"
      tabIndex={0}
      className="badge tone-error denied-chip"
      aria-label={`${plural(denied.length, "denied network request")} during this call, ${hostSummary(denied)}`}
      onClick={open}
      onKeyDown={activate(open)}
      {...tipProps(
        tip,
        <>
          <div className="tip-title">
            Denied during this call <span>{denied.length}</span>
          </div>
          <div className="tip-note">{hostSummary(denied)}</div>
        </>,
      )}
    >
      ✕ {denied.length} denied
    </span>
  );
}

export const Call = memo(function Call({ call, output, denied, onTab, onDecide, busy }: CallProps): ReactElement {
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
          {denied && <DeniedChip id={call.id} denied={denied} onTab={onTab} />}
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
