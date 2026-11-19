import type { ReactElement } from "react";
import { SubmissionStatus } from "../../gen/durable/v1/durable_pb";
import { activate } from "../../lib/a11y";
import { DataTable, Num } from "../../ui/DataTable";
import { dash, money, plural, seconds } from "../../lib/format";
import type { Overview, TurnBar } from "./overview";
import { Icon, Legend, Panel, TipRow, type Jump } from "./parts";
import { tipProps, useTip } from "../../ui/Tooltip";
import { statusText } from "../../lib/status";

function TurnTip({ t }: { t: TurnBar }): ReactElement {
  return (
    <>
      <div className="tip-title">
        Turn {t.n}
        {t.prompt && <span> · {t.prompt}</span>}
      </div>
      <TipRow label="wall" value={seconds(t.wallMs)} />
      <TipRow color="--series-model" label="model" value={seconds(t.modelMs)} />
      <TipRow color="--series-tool" label="tools" value={seconds(t.toolMs)} />
      <TipRow color="--series-other" label="other" value={t.wallMs === undefined ? dash : seconds(t.segOther)} />
      <TipRow label="cost" value={money(t.cost, t.estimated)} />
      <TipRow label="calls" value={t.calls} />
      <TipRow label="errors" value={t.errors} />
      <TipRow label="status" value={statusText[t.status] ?? dash} />
    </>
  );
}

function TurnFlag({ t }: { t: TurnBar }): ReactElement | null {
  if (t.status === SubmissionStatus.FAILED || t.errors > 0) {
    const text = t.status === SubmissionStatus.FAILED ? "failed" : plural(t.errors, "error");
    return (
      <span className="tl-flag tone-error flag">
        <Icon kind="alert" />
        {text}
      </span>
    );
  }
  if (t.status === SubmissionStatus.CANCELLED) return <span className="badge tone-cancelled">cancelled</span>;
  return null;
}

export function Timeline({ overview, onJump }: { overview: Overview; onJump: Jump }): ReactElement {
  const tip = useTip();
  const { turns } = overview;
  const max = Math.max(1, ...turns.map((t) => t.totalMs));
  const sum = (f: (t: TurnBar) => number) => turns.reduce((a, t) => a + f(t), 0);
  return (
    <Panel
      title="Turn timeline"
      table={
        <DataTable head={["Turn", "Prompt", "Status", "Wall", "Model", "Tools", "Other", "Cost", "Calls", "Errors"]} empty={turns.length ? undefined : "No turns."}>
          {turns.map((t) => (
            <tr key={String(t.seq)}>
              <td>{t.n}</td>
              <td className="wrap">{t.prompt}</td>
              <Num>{statusText[t.status] ?? dash}</Num>
              <Num>{seconds(t.wallMs)}</Num>
              <Num>{seconds(t.modelMs)}</Num>
              <Num>{seconds(t.toolMs)}</Num>
              <Num>{t.wallMs === undefined ? dash : seconds(t.segOther)}</Num>
              <Num>{money(t.cost, t.estimated)}</Num>
              <Num>{t.calls}</Num>
              <Num>{t.errors}</Num>
            </tr>
          ))}
        </DataTable>
      }
    >
      {turns.length === 0 ? (
        <p className="muted">No turns yet.</p>
      ) : (
        <>
          <Legend
            items={[
              { color: "--series-model", label: "Model", value: seconds(sum((t) => t.segModel)) },
              { color: "--series-tool", label: "Tools", value: seconds(sum((t) => t.segTool)) },
              { color: "--series-other", label: "Other", value: seconds(sum((t) => t.segOther)) },
            ]}
          />
          <div className="tl">
            {turns.map((t) => {
              const jump = () => onJump(`turn-${t.seq}`);
              return (
                <div
                  key={String(t.seq)}
                  className={`tl-row${t.status === SubmissionStatus.CANCELLED ? " tone-cancelled" : ""}`}
                  role="button"
                  tabIndex={0}
                  aria-label={`Turn ${t.n}: ${t.prompt || "no prompt"}. Open in transcript`}
                  onClick={jump}
                  onKeyDown={activate(jump)}
                  {...tipProps(tip, <TurnTip t={t} />)}
                >
                  <div className="tl-label">
                    <span className="tl-n">{t.n}</span>
                    <span className="tl-prompt" title={t.prompt}>
                      {t.prompt || <span className="faint">(no prompt)</span>}
                    </span>
                    <TurnFlag t={t} />
                  </div>
                  <div className="tl-bar">
                    {t.totalMs > 0 && (
                      <div className="tl-track bar-fill" style={{ width: `${(t.totalMs / max) * 100}%` }}>
                        <span className="seg" style={{ flexGrow: t.segModel, background: "var(--series-model)" }} />
                        <span className="seg" style={{ flexGrow: t.segTool, background: "var(--series-tool)" }} />
                        <span className="seg" style={{ flexGrow: t.segOther, background: "var(--series-other)" }} />
                      </div>
                    )}
                    <span className="tl-dur">{t.wallMs === undefined && t.totalMs === 0 ? dash : seconds(t.totalMs)}</span>
                  </div>
                  <div className="tl-meta">
                    {t.cost !== undefined && <span>{money(t.cost, t.estimated)}</span>}
                    <span>{plural(t.calls, "call")}</span>
                  </div>
                </div>
              );
            })}
          </div>
        </>
      )}
    </Panel>
  );
}
