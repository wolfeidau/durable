import { memo, type ReactElement } from "react";
import { CallLists } from "./CallLists";
import { ContextChart } from "./ContextChart";
import { Kpis } from "./Kpis";
import { ModelPanel } from "./ModelPanel";
import type { Overview } from "./overview";
import type { Jump } from "./parts";
import { Timeline } from "./Timeline";
import { TipHost } from "../../ui/Tooltip";
import { ToolChart } from "./ToolChart";
import type { Report, Summary } from "./usage";

interface Props {
  report: Report;
  overview: Overview;
  stats: Summary;
  onJump: Jump;
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
