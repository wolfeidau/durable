import type { ReactElement, ReactNode } from "react";

export const Num = ({ children }: { children: ReactNode }) => <td className="num">{children}</td>;

interface Props {
  head: string[];
  empty?: string;
  // Class after "table-wrap" on the wrapper.
  wrapClass?: string;
  // Whether every header after the first is a number column.
  numeric?: boolean;
  children: ReactNode;
}

export function DataTable({ head, empty, wrapClass = "report", numeric = true, children }: Props): ReactElement {
  return (
    <div className={`table-wrap ${wrapClass}`}>
      <table>
        <thead>
          <tr>
            {head.map((h, i) => (
              <th key={h} className={numeric && i > 0 ? "num" : undefined}>
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
