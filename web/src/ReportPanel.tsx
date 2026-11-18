import { memo, useState, type KeyboardEvent, type PointerEvent, type ReactElement, type ReactNode } from "react";
import { SubmissionStatus } from "./gen/durable/v1/durable_pb";
import { slowestCount, tokenSplit, type CallRow, type Overview, type TurnBar } from "./report";
import { TipHost, tipProps, useTip } from "./Tooltip";
import { compact, dash, money, percent, seconds, type Report, type Summary } from "./usage";

const failedCap = 20;

const statusLabel: Record<number, string> = {
  [SubmissionStatus.QUEUED]: "queued",
  [SubmissionStatus.RUNNING]: "running",
  [SubmissionStatus.DONE]: "done",
  [SubmissionStatus.FAILED]: "failed",
  [SubmissionStatus.CANCELLED]: "cancelled",
};

function Icon({ kind }: { kind: "alert" | "ok" }): ReactElement {
  return (
    <svg className="icon" viewBox="0 0 16 16" width="14" height="14" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      {kind === "alert" ? (
        <>
          <circle cx="8" cy="8" r="6.5" />
          <path d="M8 4.5v4M8 11v.2" />
        </>
      ) : (
        <>
          <circle cx="8" cy="8" r="6.5" />
          <path d="M5.2 8.2l2 2 3.6-4" />
        </>
      )}
    </svg>
  );
}

const activate = (fn: () => void) => (e: KeyboardEvent) => {
  if (e.key === "Enter" || e.key === " ") {
    e.preventDefault();
    fn();
  }
};

const Num = ({ children }: { children: ReactNode }) => <td className="num">{children}</td>;

function DataTable({ head, empty, children }: { head: string[]; empty?: string; children: ReactNode }): ReactElement {
  return (
    <div className="table-wrap report">
      <table>
        <thead>
          <tr>
            {head.map((h, i) => (
              <th key={h} className={i > 0 ? "num" : undefined}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {empty && (
            <tr>
              <td colSpan={head.length} className="muted">
                {empty}
              </td>
            </tr>
          )}
          {children}
        </tbody>
      </table>
    </div>
  );
}

function Panel({ title, table, children }: { title: string; table: ReactNode; children: ReactNode }): ReactElement {
  return (
    <section className="panel">
      <h2>{title}</h2>
      {children}
      <details className="table-view">
        <summary>Show table</summary>
        {table}
      </details>
    </section>
  );
}

const Key = ({ color }: { color: string }) => <span className="key" style={{ background: `var(${color})` }} />;

function Legend({ items }: { items: { color: string; label: string; value: string }[] }): ReactElement {
  return (
    <ul className="legend">
      {items.map((i) => (
        <li key={i.label}>
          <Key color={i.color} />
          <span>{i.label}</span>
          <span className="legend-value">{i.value}</span>
        </li>
      ))}
    </ul>
  );
}

function Tile({ label, title, children }: { label: string; title?: string; children: ReactNode }): ReactElement {
  return (
    <div className="tile" title={title}>
      <span className="stat-label">{label}</span>
      <span className="tile-value">{children}</span>
    </div>
  );
}

function Kpis({ overview, stats, turns }: { overview: Overview; stats: Summary; turns: number }): ReactElement {
  const { errors, totalCalls } = overview;
  return (
    <div className="tiles">
      <Tile label="Cost" title={stats.unknownCost ? `${stats.unknownCost} replies have no known cost` : undefined}>
        {money(stats.cost, stats.estimated)}
      </Tile>
      <Tile label="Wall time">{seconds(overview.wallMs)}</Tile>
      <Tile label="Turns">{turns}</Tile>
      <Tile label="Tool calls">{totalCalls}</Tile>
      <Tile label="Errors">
        {errors > 0 ? (
          <span className="tone-error flag">
            <Icon kind="alert" />
            {errors} · {percent(errors / totalCalls)} of calls
          </span>
        ) : (
          <span className="tone-ok flag quiet">
            <Icon kind="ok" />0 errors
          </span>
        )}
      </Tile>
      <Tile label="Cache hit">{percent(stats.cacheHit)}</Tile>
      <Tile label="Context">
        {stats.contextFraction !== undefined ? (
          <span className="meter-tile">
            <meter
              aria-label="Context used"
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
          </span>
        ) : stats.contextTokens ? (
          `${compact(stats.contextTokens)} tokens`
        ) : (
          dash
        )}
      </Tile>
    </div>
  );
}

function TipRow({ color, label, value }: { color?: string; label: string; value: ReactNode }): ReactElement {
  return (
    <div className="tip-row">
      {color ? <Key color={color} /> : <span className="key none" />}
      <strong>{value}</strong>
      <span>{label}</span>
    </div>
  );
}

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
      <TipRow label="status" value={statusLabel[t.status] ?? dash} />
    </>
  );
}

function TurnFlag({ t }: { t: TurnBar }): ReactElement | null {
  if (t.status === SubmissionStatus.FAILED || t.errors > 0) {
    const text = t.status === SubmissionStatus.FAILED ? "failed" : `${t.errors} ${t.errors === 1 ? "error" : "errors"}`;
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

function Timeline({ overview, onJump }: { overview: Overview; onJump: (target: string) => void }): ReactElement {
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
              <Num>{statusLabel[t.status] ?? dash}</Num>
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
                      <div className="tl-track" style={{ width: `${(t.totalMs / max) * 100}%` }}>
                        <span className="seg" style={{ flexGrow: t.segModel, background: "var(--series-model)" }} />
                        <span className="seg" style={{ flexGrow: t.segTool, background: "var(--series-tool)" }} />
                        <span className="seg" style={{ flexGrow: t.segOther, background: "var(--series-other)" }} />
                      </div>
                    )}
                    <span className="tl-dur">{t.wallMs === undefined && t.totalMs === 0 ? dash : seconds(t.totalMs)}</span>
                  </div>
                  <div className="tl-meta">
                    {t.cost !== undefined && <span>{money(t.cost, t.estimated)}</span>}
                    <span>
                      {t.calls} {t.calls === 1 ? "call" : "calls"}
                    </span>
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

const W = 640;
const H = 220;
const padL = 48;
const padR = 34;
const padT = 12;
const padB = 24;
const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));

function ContextChart({ overview, stats, onJump }: { overview: Overview; stats: Summary; onJump: (target: string) => void }): ReactElement {
  const tip = useTip();
  const [active, setActive] = useState<number>();
  const { context: pts, compactions } = overview;
  const n = pts.length;
  const win = stats.contextWindow;
  const peak = Math.max(0, ...pts.map((p) => p.tokens));
  // Scaling a low peak to the full window flattens the line, so the window is drawn only once it is near.
  const fits = win !== undefined && peak >= 0.5 * win;
  const top = (fits ? win : peak * 1.2) || 1;
  const pw = W - padL - padR;
  const ph = H - padT - padB;
  const x = (i: number) => padL + (n === 1 ? 0.5 : i / (n - 1)) * pw;
  const y = (v: number) => padT + (1 - v / top) * ph;
  const base = y(0);

  const readout = (i: number): ReactElement => {
    const p = pts[i]!;
    const compacted = compactions.some((c) => c.at === i);
    return (
      <>
        <div className="tip-title">Reply {i + 1}</div>
        <TipRow color="--series-context" label="context tokens" value={p.tokens.toLocaleString()} />
        {p.window && <TipRow label="of window" value={percent(p.tokens / p.window)} />}
        <TipRow label="turn" value={p.n || dash} />
        {compacted && <div className="tip-note">Compacted before this reply</div>}
      </>
    );
  };

  const select = (i: number, cx: number, cy: number) => {
    setActive(i);
    tip.show(cx, cy, readout(i));
  };

  const onMove = (e: PointerEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    const px = ((e.clientX - r.left) / r.width) * W;
    select(n === 1 ? 0 : clamp(Math.round(((px - padL) / pw) * (n - 1)), 0, n - 1), e.clientX, e.clientY);
  };

  const onKey = (e: KeyboardEvent<SVGSVGElement>) => {
    const cur = active ?? n - 1;
    const next = e.key === "ArrowLeft" ? cur - 1 : e.key === "ArrowRight" ? cur + 1 : e.key === "Home" ? 0 : e.key === "End" ? n - 1 : undefined;
    if (next !== undefined) {
      e.preventDefault();
      const i = clamp(next, 0, n - 1);
      const r = e.currentTarget.getBoundingClientRect();
      select(i, r.left + (x(i) / W) * r.width, r.top + (y(pts[i]!.tokens) / H) * r.height);
    } else if (e.key === "Enter" && active !== undefined) {
      onJump(`turn-${pts[active]!.seq}`);
    }
  };

  const hide = () => {
    setActive(undefined);
    tip.hide();
  };

  const line = pts.map((p, i) => `${i ? "L" : "M"}${x(i).toFixed(1)} ${y(p.tokens).toFixed(1)}`).join("");
  const xc = (at: number) => x(clamp(at - 0.5, 0, n - 1));
  const lastP = pts[n - 1];

  return (
    <Panel
      title="Context growth"
      table={
        <DataTable head={["Reply", "Turn", "Context tokens", "Of window"]} empty={n ? undefined : "No context sizes recorded."}>
          {pts.map((p) => (
            <tr key={p.index}>
              <td>
                {p.index + 1}
                {compactions.some((c) => c.at === p.index) && " (after compaction)"}
              </td>
              <Num>{p.n || dash}</Num>
              <Num>{p.tokens.toLocaleString()}</Num>
              <Num>{p.window ? percent(p.tokens / p.window) : dash}</Num>
            </tr>
          ))}
        </DataTable>
      }
    >
      {n === 0 ? (
        <p className="muted">No context size recorded yet.</p>
      ) : (
        <svg
          className="chart"
          viewBox={`0 0 ${W} ${H}`}
          tabIndex={0}
          role="group"
          aria-label={`Context tokens per reply, ${n} replies, latest ${compact(lastP!.tokens)}${win ? ` of ${compact(win)}` : ""}. Arrow keys move between replies, Enter opens the turn.`}
          onPointerMove={onMove}
          onPointerLeave={hide}
          onFocus={(e) => {
            if (active !== undefined) return;
            const r = e.currentTarget.getBoundingClientRect();
            select(n - 1, r.left + (x(n - 1) / W) * r.width, r.top + (y(lastP!.tokens) / H) * r.height);
          }}
          onBlur={hide}
          onKeyDown={onKey}
          onClick={() => active !== undefined && onJump(`turn-${pts[active]!.seq}`)}
        >
          {fits && (
            <>
              <rect className="band" x={padL} y={y(win)} width={pw} height={y(0.9 * win) - y(win)} opacity="0.1" />
              <rect className="band" x={padL} y={y(0.9 * win)} width={pw} height={y(0.7 * win) - y(0.9 * win)} opacity="0.05" />
              <text className="axis" x={W - padR + 4} y={y(0.9 * win) + 3}>
                90%
              </text>
              <text className="axis" x={W - padR + 4} y={y(0.7 * win) + 3}>
                70%
              </text>
            </>
          )}
          {[0, 0.5, 1].map((f) => (
            <g key={f}>
              <line className="grid" x1={padL} x2={W - padR} y1={y(top * f)} y2={y(top * f)} />
              <text className="axis" x={padL - 6} y={y(top * f) + 3} textAnchor="end">
                {compact(top * f)}
              </text>
            </g>
          ))}
          {win && (
            <text className="axis" x={W - padR} y={fits ? y(win) - 3 : padT - 3} textAnchor="end">
              {fits ? `window ${compact(win)}` : `peak ${percent(peak / win)} of the ${compact(win)} window`}
            </text>
          )}
          <path d={`${line}L${x(n - 1).toFixed(1)} ${base}L${x(0).toFixed(1)} ${base}Z`} className="area" />
          <path d={line} className="line" />
          {n <= 60 && pts.map((p, i) => <circle key={i} className="dot" cx={x(i)} cy={y(p.tokens)} r={2} />)}
          {compactions.map((c, i) => (
            <g key={i}>
              <line className="compaction" x1={xc(c.at)} x2={xc(c.at)} y1={padT} y2={base} />
              <text className="axis" x={xc(c.at) + 4} y={padT + 9}>
                compacted
              </text>
            </g>
          ))}
          <text className="axis" x={padL} y={H - 6}>
            reply 1
          </text>
          <text className="axis" x={W - padR} y={H - 6} textAnchor="end">
            reply {n}
          </text>
          {active !== undefined && (
            <g className="cursor">
              <line x1={x(active)} x2={x(active)} y1={padT} y2={base} />
              <circle cx={x(active)} cy={y(pts[active]!.tokens)} r={4} />
            </g>
          )}
        </svg>
      )}
    </Panel>
  );
}

function ToolChart({ report, overview, onJump }: { report: Report; overview: Overview; onJump: (target: string) => void }): ReactElement {
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
            return (
              <div
                key={t.name}
                className={`tools-row${jump ? " link" : ""}`}
                role={jump ? "button" : undefined}
                tabIndex={jump ? 0 : undefined}
                onClick={jump}
                onKeyDown={jump && activate(jump)}
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
                  {t.total !== undefined && <span className="tools-fill" style={{ width: `${(t.total / max) * 100}%` }} />}
                  <span className="tools-total">{seconds(t.total)}</span>
                </span>
                <span className="num">{t.calls}</span>
                <span className="num">
                  <span className={`badge ${t.errors > 0 ? "tone-error" : "tone-muted quiet"}`}>{t.errors}</span>
                </span>
                <span className="num">{seconds(t.avg)}</span>
                <span className="num">{seconds(t.p95)}</span>
              </div>
            );
          })}
        </div>
      )}
    </Panel>
  );
}

function CallList({ rows, empty, onJump, failed }: { rows: CallRow[]; empty: ReactNode; onJump: (target: string) => void; failed?: boolean }): ReactElement {
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

function CallLists({ overview, onJump }: { overview: Overview; onJump: (target: string) => void }): ReactElement {
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

const splitColor = { input: "--series-input", cacheRead: "--series-cache-read", cacheWrite: "--series-cache-write", output: "--series-output" } as const;

function ModelPanel({ report, stats }: { report: Report; stats: Summary }): ReactElement {
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

interface Props {
  report: Report;
  overview: Overview;
  stats: Summary;
  onJump: (target: string) => void;
}

export const ReportPanel = memo(function ReportPanel({ report, overview, stats, onJump }: Props): ReactElement {
  return (
    <TipHost>
      <div className="report-page">
        <Kpis overview={overview} stats={stats} turns={overview.turns.length} />
        <Timeline overview={overview} onJump={onJump} />
        <ContextChart overview={overview} stats={stats} onJump={onJump} />
        <ToolChart report={report} overview={overview} onJump={onJump} />
        <CallLists overview={overview} onJump={onJump} />
        <ModelPanel report={report} stats={stats} />
      </div>
    </TipHost>
  );
});
