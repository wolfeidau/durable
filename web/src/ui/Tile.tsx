import type { ReactElement, ReactNode } from "react";

export function Tile({ label, title, className, children }: { label: string; title?: string; className?: string; children: ReactNode }): ReactElement {
  return (
    <div className={className ? `tile ${className}` : "tile"} title={title}>
      <span className="stat-label">{label}</span>
      <span className="tile-value">{children}</span>
    </div>
  );
}
