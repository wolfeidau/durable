import type { ReactElement } from "react";
import { DataTable, Num } from "../../ui/DataTable";
import { compact, dash, percent, seconds } from "../../lib/format";
import { tokenSplit } from "./overview";
import { Legend, Panel, TipRow } from "./parts";
import { tipProps, useTip } from "../../ui/Tooltip";
import type { Report, Summary } from "./usage";

const splitColor = { input: "--series-input", cacheRead: "--series-cache-read", cacheWrite: "--series-cache-write", output: "--series-output" } as const;

export function ModelPanel({ report, stats }: { report: Report; stats: Summary }): ReactElement {
  const tip = useTip();
  const { model } = report;
  const parts = tokenSplit(stats);
  const total = parts.reduce((a, p) => a + p.tokens, 0);
  const share = (tokens: number) => (total > 0 ? tokens / total : 0);
  return (
    <Panel
      title="Model"
      table={
        <>
          <DataTable head={["Replies", "Avg", "p95", "Output tokens/s"]}>
            <tr>
              <td>{model.replies}</td>
              <Num>{seconds(model.avg)}</Num>
              <Num>{seconds(model.p95)}</Num>
              <Num>{model.tokensPerSecond === undefined ? dash : compact(model.tokensPerSecond)}</Num>
            </tr>
          </DataTable>
          <DataTable head={["Tokens", "Count", "Share"]}>
            {parts.map((p) => (
              <tr key={p.key}>
                <td>{p.label}</td>
                <Num>{p.tokens.toLocaleString()}</Num>
                <Num>{total > 0 ? percent(share(p.tokens)) : dash}</Num>
              </tr>
            ))}
          </DataTable>
        </>
      }
    >
      <dl className="facts">
        <div>
          <dt>Replies</dt>
          <dd>{model.replies}</dd>
        </div>
        <div>
          <dt>Avg latency</dt>
          <dd>{seconds(model.avg)}</dd>
        </div>
        <div>
          <dt>p95 latency</dt>
          <dd>{seconds(model.p95)}</dd>
        </div>
        <div>
          <dt>Output tokens/s</dt>
          <dd>{model.tokensPerSecond === undefined ? dash : compact(model.tokensPerSecond)}</dd>
        </div>
      </dl>
      {total === 0 ? (
        <p className="muted">No token usage recorded.</p>
      ) : (
        <>
          <div className="split" role="img" aria-label={parts.map((p) => `${p.label} ${compact(p.tokens)}`).join(", ")}>
            {parts.map((p) => (
              <span
                key={p.key}
                className="seg"
                style={{ flexGrow: p.tokens, background: `var(${splitColor[p.key]})` }}
                {...tipProps(tip, <TipRow color={splitColor[p.key]} label={p.label} value={`${p.tokens.toLocaleString()} · ${percent(share(p.tokens))}`} />)}
              />
            ))}
          </div>
          <Legend items={parts.map((p) => ({ color: splitColor[p.key], label: p.label, value: `${compact(p.tokens)} · ${percent(share(p.tokens))}` }))} />
        </>
      )}
    </Panel>
  );
}
