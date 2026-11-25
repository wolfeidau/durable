import { useState, type KeyboardEvent, type PointerEvent, type ReactElement } from "react";
import { DataTable, Num } from "../../ui/DataTable";
import { compact, dash, percent } from "../../lib/format";
import type { Overview } from "./overview";
import { Panel, TipRow, type Jump } from "./parts";
import { useTip } from "../../ui/Tooltip";
import type { Summary } from "./usage";

const W = 640;
const H = 230;
const padL = 58;
const padR = 44;
const padT = 22;
const padB = 28;
const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));

export function ContextChart({ overview, stats, onJump }: { overview: Overview; stats: Summary; onJump: Jump }): ReactElement {
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
            <text className="axis" x={W - padR} y={fits ? y(win) - 6 : padT - 6} textAnchor="end">
              {fits ? `window ${compact(win)}` : `peak ${percent(peak / win)} of the ${compact(win)} window`}
            </text>
          )}
          <path d={`${line}L${x(n - 1).toFixed(1)} ${base}L${x(0).toFixed(1)} ${base}Z`} className="area" />
          <path d={line} className="line" />
          {n <= 60 && pts.map((p, i) => <circle key={i} className="dot" cx={x(i)} cy={y(p.tokens)} r={2} />)}
          {compactions.map((c, i) => {
            const cx = xc(c.at);
            const flip = cx > padL + pw * 0.6;
            return (
              <g key={i}>
                <line className="compaction" x1={cx} x2={cx} y1={padT} y2={base} />
                <text className="axis" x={flip ? cx - 5 : cx + 5} y={padT + 16} textAnchor={flip ? "end" : "start"}>
                  compacted
                </text>
              </g>
            );
          })}
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
