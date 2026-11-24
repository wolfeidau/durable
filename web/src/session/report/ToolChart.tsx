import type { ReactElement } from "react";
import { DataTable, Num } from "../../ui/DataTable";
import { seconds } from "../../lib/format";
import type { Overview } from "./overview";
import { Panel, TipRow, type Jump } from "./parts";
import { tipProps, useTip } from "../../ui/Tooltip";
import type { Report } from "./usage";

export function ToolChart({ report, overview, onJump }: { report: Report; overview: Overview; onJump: Jump }): ReactElement {
  const tip = useTip();
  const { tools } = report;
  const max = Math.max(1, ...tools.map((t) => t.total ?? 0));
  return (
    <Panel
      title="Tools"
      table={
        <DataTable head={["Tool", "Calls", "Errors", "Total", "Avg", "p95", "Max"]} empty={tools.length ? undefined : "No tool calls."}>
          {tools.map((t) => (
            <tr key={t.name}>
              <td>{t.name}</td>
              <Num>{t.calls}</Num>
              <Num>{t.errors}</Num>
              <Num>{seconds(t.total)}</Num>
              <Num>{seconds(t.avg)}</Num>
              <Num>{seconds(t.p95)}</Num>
              <Num>{seconds(t.max)}</Num>
            </tr>
          ))}
        </DataTable>
      }
    >
      {tools.length === 0 ? (
        <p className="muted">No tool calls.</p>
      ) : (
        <div className="tools">
          <div className="tools-row tools-head" aria-hidden="true">
            <span>Tool</span>
            <span className="tools-bar">Total time</span>
            <span className="num">Calls</span>
            <span className="num">Errors</span>
            <span className="num">Avg</span>
            <span className="num">p95</span>
          </div>
          {tools.map((t) => {
            const named = overview.calls.filter((c) => c.name === t.name);
            const target = named.filter((c) => c.ms !== undefined).sort((a, b) => b.ms! - a.ms!)[0] ?? named[0];
            const jump = target ? () => onJump(`call-${target.id}`) : undefined;
            const Row = jump ? "button" : "div";
            return (
              <Row
                key={t.name}
                className={`tools-row${jump ? " row-btn link" : ""}`}
                type={jump ? "button" : undefined}
                onClick={jump}
                {...tipProps(
                  tip,
                  <>
                    <div className="tip-title">{t.name}</div>
                    <TipRow color="--series-tool" label="total" value={seconds(t.total)} />
                    <TipRow label="calls" value={t.calls} />
                    <TipRow label="errors" value={t.errors} />
                    <TipRow label="avg" value={seconds(t.avg)} />
                    <TipRow label="p95" value={seconds(t.p95)} />
                    <TipRow label="max" value={seconds(t.max)} />
                    {jump && <div className="tip-note">Opens the slowest call</div>}
                  </>,
                )}
              >
                <span className="tools-name">{t.name}</span>
                <span className="tools-bar">
                  {t.total !== undefined && <span className="tools-fill bar-fill" style={{ width: `${(t.total / max) * 100}%` }} />}
                  <span className="tools-total">{seconds(t.total)}</span>
                </span>
                <span className="num">{t.calls}</span>
                <span className="num">
                  <span className={`badge ${t.errors > 0 ? "tone-error" : "tone-muted quiet"}`}>{t.errors}</span>
                </span>
                <span className="num">{seconds(t.avg)}</span>
                <span className="num">{seconds(t.p95)}</span>
              </Row>
            );
          })}
        </div>
      )}
    </Panel>
  );
}
