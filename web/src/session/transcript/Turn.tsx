import { memo, type ReactElement } from "react";
import type { NetworkEvent } from "../../gen/durable/v1/durable_pb";
import { CopyButton } from "../../ui/CopyButton";
import { isActive, statusText, statusTone } from "../../lib/status";
import { Call, type OpenTab } from "./Call";
import { Markdown } from "./markdown";
import type { Turn } from "./transcript";

const noOutput: ReadonlyMap<string, string> = new Map();

interface TurnProps {
  turn: Turn;
  usage?: string;
  localText?: string;
  partial?: string;
  toolOutput: ReadonlyMap<string, string>;
  denied: ReadonlyMap<string, NetworkEvent[]>;
  onTab: OpenTab;
  onDecide: (seq: bigint, allow: boolean) => void;
  onCancel: (requestId: string) => void;
  busy: boolean;
}

export const TurnView = memo(function TurnView({ turn, usage, localText, partial, toolOutput, denied, onTab, onDecide, onCancel, busy }: TurnProps): ReactElement {
  const hasUser = turn.entries.some((e) => e.kind === "user");
  const { requestId } = turn;
  return (
    <section className="turn" id={`turn-${turn.seq}`}>
      {!hasUser && localText && <div className="msg user">{localText}</div>}
      {turn.entries.map((e, i) => {
        if (e.kind === "call") {
          return <Call key={i} call={e} output={toolOutput.get(e.id)} denied={denied.get(e.id)} onTab={onTab} onDecide={onDecide} busy={busy} />;
        }
        if (e.kind === "assistant") {
          return (
            <div key={i} className="msg assistant">
              <Markdown text={e.text} />
              <div className="msg-actions">
                <CopyButton text={e.text} />
              </div>
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

export const PendingTurn = memo(function PendingTurn({ id, prompt, onCancel }: { id: string; prompt: string; onCancel: (requestId: string) => void }): ReactElement {
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
export function liveOutput(turn: Turn, all: ReadonlyMap<string, string>): ReadonlyMap<string, string> {
  let out: Map<string, string> | undefined;
  for (const e of turn.entries) {
    if (e.kind !== "call" || e.result !== undefined) continue;
    const o = all.get(e.id);
    if (o !== undefined) (out ??= new Map()).set(e.id, o);
  }
  return out ?? noOutput;
}
