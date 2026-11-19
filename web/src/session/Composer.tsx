import type { KeyboardEvent, ReactElement, Ref } from "react";
import { money, percent } from "../lib/format";
import { useNow } from "../ui/useNow";
import { ContextMeter } from "./ContextMeter";
import type { Summary } from "./report/usage";
import { activityText } from "./transcript/activity";
import type { Turn } from "./transcript/transcript";

function Activity({ turn }: { turn: Turn }): ReactElement {
  const now = useNow(true);
  return <span className="tone-link activity">{activityText(turn, now)}</span>;
}

interface Props {
  text: string;
  onText: (text: string) => void;
  onSend: () => void;
  note: string;
  stats: Summary;
  latest?: Turn;
  ref: Ref<HTMLDivElement>;
}

export function Composer({ text, onText, onSend, note, stats, latest, ref }: Props): ReactElement {
  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      onSend();
    }
  };

  return (
    <div className="composer" ref={ref}>
      {note && <p className="banner tone-error">{note}</p>}
      <textarea
        value={text}
        rows={3}
        placeholder="Message"
        aria-label="Message"
        onChange={(e) => onText(e.target.value)}
        onKeyDown={onKey}
      />
      <div className="composer-foot">
        <div className="stats">
          <ContextMeter stats={stats} variant="composer" />
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
          <button disabled={!text.trim()} onClick={onSend}>
            Submit
          </button>
        </div>
      </div>
    </div>
  );
}
