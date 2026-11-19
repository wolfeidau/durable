import type { ReactElement } from "react";
import { ContextMeter } from "../ContextMeter";
import { money, percent, seconds } from "../../lib/format";
import { Tile } from "../../ui/Tile";
import type { Overview } from "./overview";
import { Icon } from "./parts";
import type { Summary } from "./usage";

export function Kpis({ overview, stats, turns }: { overview: Overview; stats: Summary; turns: number }): ReactElement {
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
        <ContextMeter stats={stats} variant="tile" />
      </Tile>
    </div>
  );
}
