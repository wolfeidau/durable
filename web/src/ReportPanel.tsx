import { memo, type ReactElement, type ReactNode } from "react";
import { compact, dash, money, seconds, type Report } from "./usage";

function Table({ title, head, children }: { title: string; head: string[]; children: ReactNode }): ReactElement {
  return (
    <section>
      <h2>{title}</h2>
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
          <tbody>{children}</tbody>
        </table>
      </div>
    </section>
  );
}

const Num = ({ children }: { children: ReactNode }) => <td className="num">{children}</td>;

export const ReportPanel = memo(function ReportPanel({ report }: { report: Report }): ReactElement {
  const { tools, model, turns } = report;
  return (
    <div className="report">
      <Table title="Tools" head={["Tool", "Calls", "Errors", "Total", "Avg", "p95", "Max"]}>
        {tools.length === 0 && (
          <tr>
            <td colSpan={7} className="muted">
              No tool calls.
            </td>
          </tr>
        )}
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
      </Table>
      <Table title="Model" head={["Replies", "Avg", "p95", "Output tokens/s"]}>
        <tr>
          <Num>{model.replies}</Num>
          <Num>{seconds(model.avg)}</Num>
          <Num>{seconds(model.p95)}</Num>
          <Num>{model.tokensPerSecond === undefined ? dash : compact(model.tokensPerSecond)}</Num>
        </tr>
      </Table>
      <Table title="Turns" head={["Turn", "Cost", "Wall", "Calls", "Model", "Tools"]}>
        {turns.length === 0 && (
          <tr>
            <td colSpan={6} className="muted">
              No turns.
            </td>
          </tr>
        )}
        {turns.map((t, i) => (
          <tr key={String(t.seq)}>
            <td>{i + 1}</td>
            <Num>{money(t.cost, t.estimated)}</Num>
            <Num>{seconds(t.wallMs)}</Num>
            <Num>{t.calls}</Num>
            <Num>{seconds(t.modelMs)}</Num>
            <Num>{seconds(t.toolMs)}</Num>
          </tr>
        ))}
      </Table>
    </div>
  );
});
