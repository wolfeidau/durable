import type { ReactElement } from "react";
import { compact, dash, percent } from "../lib/format";
import type { Summary } from "./report/usage";

function Gauge({ stats, label }: { stats: Summary; label: { "aria-label": string } | { "aria-labelledby": string } }): ReactElement | null {
  if (stats.contextFraction === undefined) return null;
  return (
    <>
      <meter
        {...label}
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
    </>
  );
}

const tokensText = (stats: Summary): string => (stats.contextTokens ? `${compact(stats.contextTokens)} tokens` : dash);

export function ContextMeter({ stats, variant }: { stats: Summary; variant: "composer" | "tile" }): ReactElement {
  const known = stats.contextFraction !== undefined;
  if (variant === "tile") {
    return known ? (
      <span className="meter-tile">
        <Gauge stats={stats} label={{ "aria-label": "Context used" }} />
      </span>
    ) : (
      <>{tokensText(stats)}</>
    );
  }
  return (
    <div className="meter">
      <span className="stat-label" id="ctx-label">
        Context
      </span>
      {known ? <Gauge stats={stats} label={{ "aria-labelledby": "ctx-label" }} /> : <span>{tokensText(stats)}</span>}
    </div>
  );
}
