import type { ReactElement, ReactNode } from "react";

export function Tile({ label, title, children }: { label: string; title?: string; children: ReactNode }): ReactElement {
  return (
    <div className="tile" title={title}>
      <span className="stat-label">{label}</span>
      <span className="tile-value">{children}</span>
    </div>
  );
}
