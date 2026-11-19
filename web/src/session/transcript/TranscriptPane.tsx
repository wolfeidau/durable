import type { ReactElement, Ref } from "react";
import type { NetworkEvent } from "../../gen/durable/v1/durable_pb";
import { TipHost } from "../../ui/Tooltip";
import { FocusTarget, OpenCalls, type OpenTab } from "./Call";
import { liveOutput, PendingTurn, TurnView } from "./Turn";
import type { Turn } from "./transcript";

interface Props {
  turns: Turn[];
  footers: ReadonlyMap<bigint, string>;
  mine: ReadonlyMap<string, string>;
  partialText: ReadonlyMap<bigint, string>;
  toolOutput: ReadonlyMap<string, string>;
  denied: ReadonlyMap<string, NetworkEvent[]>;
  onTab: OpenTab;
  onDecide: (seq: bigint, allow: boolean) => void;
  onCancel: (requestId: string) => void;
  busy: boolean;
  unseen: [string, string][];
  loading: boolean;
  calls: { open: boolean; version: number };
  focus?: string;
  hidden: boolean;
  ref: Ref<HTMLDivElement>;
}

export function TranscriptPane({ turns, footers, mine, partialText, toolOutput, denied, onTab, onDecide, onCancel, busy, unseen, loading, calls, focus, hidden, ref }: Props): ReactElement {
  return (
    <TipHost>
      <FocusTarget.Provider value={focus}>
        <OpenCalls.Provider value={calls}>
          <div className="transcript" ref={ref} hidden={hidden}>
            {turns.map((t) => (
              <TurnView
                key={String(t.seq)}
                turn={t}
                usage={footers.get(t.seq)}
                localText={t.requestId ? mine.get(t.requestId) : undefined}
                partial={partialText.get(t.seq)}
                toolOutput={liveOutput(t, toolOutput)}
                denied={denied}
                onTab={onTab}
                onDecide={onDecide}
                onCancel={onCancel}
                busy={busy}
              />
            ))}
            {unseen.map(([id, prompt]) => (
              <PendingTurn key={id} id={id} prompt={prompt} onCancel={onCancel} />
            ))}
            {loading && <p className="muted">Loading…</p>}
            {!loading && turns.length === 0 && unseen.length === 0 && <p className="muted">No messages yet.</p>}
          </div>
        </OpenCalls.Provider>
      </FocusTarget.Provider>
    </TipHost>
  );
}
