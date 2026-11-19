import type { ReactElement, ReactNode } from "react";

export type Jump = (target: string) => void;

export function Icon({ kind }: { kind: "alert" | "ok" }): ReactElement {
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

export function Panel({ title, table, children }: { title: string; table: ReactNode; children: ReactNode }): ReactElement {
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

export function Legend({ items }: { items: { color: string; label: string; value: string }[] }): ReactElement {
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

export function TipRow({ color, label, value }: { color?: string; label: string; value: ReactNode }): ReactElement {
  return (
    <div className="tip-row">
      {color ? <Key color={color} /> : <span className="key none" />}
      <strong>{value}</strong>
      <span>{label}</span>
    </div>
  );
}
