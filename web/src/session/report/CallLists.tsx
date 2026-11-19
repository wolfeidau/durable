import type { ReactElement, ReactNode } from "react";
import { activate } from "../../lib/a11y";
import { DataTable, Num } from "../../ui/DataTable";
import { seconds } from "../../lib/format";
import { slowestCount, type CallRow, type Overview } from "./overview";
import { Icon, Panel, TipRow, type Jump } from "./parts";
import { tipProps, useTip } from "../../ui/Tooltip";

const failedCap = 20;

function CallList({ rows, empty, onJump, failed }: { rows: CallRow[]; empty: ReactNode; onJump: Jump; failed?: boolean }): ReactElement {
  const tip = useTip();
  if (rows.length === 0) return <>{empty}</>;
  return (
    <ul className="calls">
      {rows.map((c) => {
        const jump = () => onJump(`call-${c.id}`);
        return (
          <li
            key={c.id}
            role="button"
            tabIndex={0}
            onClick={jump}
            onKeyDown={activate(jump)}
            {...tipProps(
              tip,
              <>
                <div className="tip-title">
                  {c.name} · turn {c.n}
                </div>
                {c.preview && <div className="tip-note">{c.preview}</div>}
                <TipRow label="duration" value={seconds(c.ms)} />
                {c.error && <div className="tip-note">{c.error}</div>}
                <div className="tip-note">Opens the call</div>
              </>,
            )}
          >
            <span className="call-name">{c.name}</span>
            <span className="calls-preview">{c.preview}</span>
            <span className="calls-right">{seconds(c.ms)}</span>
            {failed && <span className="calls-error">{c.error || "(no output)"}</span>}
          </li>
        );
      })}
    </ul>
  );
}

function CallRows({ rows }: { rows: CallRow[] }): ReactElement {
  return (
    <>
      {rows.map((c) => (
        <tr key={c.id}>
          <td>{c.name}</td>
          <Num>{c.n}</Num>
          <td className="wrap">{c.preview}</td>
          <Num>{seconds(c.ms)}</Num>
          <td className="wrap">{c.error}</td>
        </tr>
      ))}
    </>
  );
}

const callHead = ["Tool", "Turn", "Input", "Duration", "Error"];

export function CallLists({ overview, onJump }: { overview: Overview; onJump: Jump }): ReactElement {
  const { slowest, failed } = overview;
  return (
    <div className="pair">
      <Panel title={`Slowest calls`} table={<DataTable head={callHead} empty={slowest.length ? undefined : "No timed calls."}><CallRows rows={slowest} /></DataTable>}>
        <CallList rows={slowest} empty={<p className="muted">No timed calls.</p>} onJump={onJump} />
        {slowest.length > 0 && <p className="faint note">Top {slowestCount} by duration.</p>}
      </Panel>
      <Panel title="Failed calls" table={<DataTable head={callHead} empty={failed.length ? undefined : "No failed calls."}><CallRows rows={failed} /></DataTable>}>
        <CallList
          rows={failed.slice(0, failedCap)}
          failed
          empty={
            <p className="tone-ok flag">
              <Icon kind="ok" />
              No failed calls
            </p>
          }
          onJump={onJump}
        />
        {failed.length > failedCap && <p className="muted note">and {failed.length - failedCap} more, newest first in the table.</p>}
      </Panel>
    </div>
  );
}
