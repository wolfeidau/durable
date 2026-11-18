import { memo, useMemo, useState, type ReactElement } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { NetworkEvent } from "../../gen/durable/v1/durable_pb";
import { summary } from "../transcript/calls";
import { coverageNotes, networkSourceLabel, type NetworkState } from "./network";
import type { CallEntry } from "../transcript/transcript";
import { eventMs, matches, rowCap, topDenied, type Verdict } from "./networkView";
import { DataTable } from "../../ui/DataTable";
import { dash } from "../../lib/format";
import { Tile } from "../../ui/Tile";
import { useNow } from "../../ui/useNow";

interface Props {
  state: NetworkState | undefined;
  calls: CallEntry[];
  denied: Map<string, NetworkEvent[]>;
  call?: string;
  onCall: (id: string | undefined) => void;
  onJump: (target: string) => void;
}

export const noSandboxText = "This session has no sandbox, so it has no network decisions.";

const sameDay = (a: Date, b: Date) => a.toDateString() === b.toDateString();
const clock = (d: Date) => d.toLocaleTimeString([], { hour12: false });

function fmtTime(d: Date): string {
  return sameDay(d, new Date()) ? clock(d) : `${d.toLocaleDateString()} ${clock(d)}`;
}

const verdicts: { value: Verdict; label: string }[] = [
  { value: "all", label: "All" },
  { value: "denied", label: "Denied" },
  { value: "allowed", label: "Allowed" },
];

export const NetworkPanel = memo(function NetworkPanel({ state, calls, denied, call, onCall, onJump }: Props): ReactElement {
  const [verdict, setVerdict] = useState<Verdict>("all");
  const [text, setText] = useState("");
  const [limit, setLimit] = useState(rowCap);
  const selected = call ? calls.find((c) => c.id === call) : undefined;
  const now = useNow(selected !== undefined && selected.doneAt === undefined);
  const events = state?.events;

  const stats = useMemo(() => {
    const list = events ?? [];
    const allowed = list.filter((e) => e.allowed).length;
    return { allowed, denied: list.length - allowed, hosts: new Set(list.map((e) => e.host)).size };
  }, [events]);
  const top = useMemo(() => topDenied(events ?? []), [events]);
  const owner = useMemo(() => {
    const m = new Map<string, string>();
    for (const [id, list] of denied) for (const e of list) if (!m.has(e.id)) m.set(e.id, id);
    return m;
  }, [denied]);

  const range = selected?.startedAt === undefined ? undefined : { start: selected.startedAt, end: selected.doneAt ?? now };
  const rows = useMemo(() => {
    const f = { verdict, text, window: range };
    return (events ?? []).filter((e) => matches(e, f)).reverse();
  }, [events, verdict, text, range?.start, range?.end]);

  if (!state || state.status === "loading") return <p className="muted">Loading…</p>;
  if (state.status === "no-sandbox") return <p className="muted">{noSandboxText}</p>;

  const notes = coverageNotes(state, fmtTime);
  const since = fmtTime(state.since);
  const filtered = verdict !== "all" || text.trim() !== "" || call !== undefined;
  const shown = rows.slice(0, limit);

  return (
    <div className="network-page">
      {state.error && <p className="banner tone-error">{state.error}</p>}
      {notes.length > 0 && (
        <div className={`banner ${notes.some((n) => n.tone === "error") ? "tone-error" : "tone-warn"}`}>
          <ul className="notes">
            {notes.map((n) => (
              <li key={n.text}>{n.text}</li>
            ))}
          </ul>
        </div>
      )}
      <div className="tiles">
        <Tile label="Decisions">{state.events.length}</Tile>
        <Tile label="Allowed">
          <span className="tone-ok flag">✓ {stats.allowed}</span>
        </Tile>
        <Tile label="Denied">
          <span className="tone-error flag">✕ {stats.denied}</span>
        </Tile>
        <Tile label="Hosts">{stats.hosts}</Tile>
        <Tile label="Source">
          {state.coverage ? networkSourceLabel[state.coverage.source] : dash}
          {state.coverage?.since && <span className="tile-sub">since {fmtTime(timestampDate(state.coverage.since))}</span>}
        </Tile>
      </div>
      {top.length > 0 && (
        <section className="panel">
          <h2>Top denied</h2>
          <ul className="top-denied">
            {top.map((t) => (
              <li key={`${t.host}\0${t.what}`}>
                <button className="link-btn" title={`Filter by ${t.host}`} onClick={() => setText(t.host)}>
                  <span className="mono top-host">{t.host || "unknown host"}</span>
                  {t.what && <span className="mono muted top-what">{t.what}</span>}
                  <span className="badge tone-error">✕ {t.count}</span>
                </button>
              </li>
            ))}
          </ul>
        </section>
      )}
      <div className="net-filters">
        <div className="tabs net-verdicts" role="group" aria-label="Verdict">
          {verdicts.map((v) => (
            <button key={v.value} className="secondary" aria-pressed={verdict === v.value} onClick={() => setVerdict(v.value)}>
              {v.label}
            </button>
          ))}
        </div>
        <input type="search" value={text} placeholder="Filter by host or path" aria-label="Filter by host or path" onChange={(e) => setText(e.target.value)} />
        {call && (
          <button className="secondary call-filter" title="Show all calls" onClick={() => onCall(undefined)}>
            Call: {selected ? [selected.name, summary(selected)].filter(Boolean).join(" · ") : call} ×
          </button>
        )}
      </div>
      {rows.length === 0 ? (
        <p className="muted">{filtered ? "No decisions match the filters." : `No network decisions since ${since}.`}</p>
      ) : (
        <DataTable head={["Time", "Verdict", "Request", "Policy"]} wrapClass="report net-table" numeric={false}>
              {shown.map((e) => {
                const t = new Date(eventMs(e));
                const owned = owner.get(e.id);
                return (
                  <tr key={e.id}>
                    <td className="time" title={t.toISOString()}>
                      {fmtTime(t)}
                    </td>
                    <td>
                      <span className={`badge ${e.allowed ? "tone-ok" : "tone-error"}`}>{e.allowed ? "✓ allowed" : "✕ denied"}</span>
                      {owned && (
                        <button className="link-btn call-link" onClick={() => onJump(`call-${owned}`)}>
                          → call
                        </button>
                      )}
                    </td>
                    <td className="request wrap">
                      <span className="mono">
                        {e.host}
                        {e.path && (
                          <span className="muted" title={e.path}>
                            {e.pattern || e.path}
                          </span>
                        )}
                      </span>
                      <span className="faint">
                        {e.kind}
                        {(e.method || e.binary) && ` · ${e.method || e.binary}`}
                      </span>
                    </td>
                    <td className="policy wrap">
                      {e.policy && <span>{e.policy}</span>}
                      {e.reason && (
                        <span className="faint" title={e.reason}>
                          {e.reason}
                        </span>
                      )}
                    </td>
                  </tr>
                );
              })}
        </DataTable>
      )}
      {rows.length > shown.length && (
        <button className="secondary" onClick={() => setLimit((l) => l + rowCap)}>
          Show {Math.min(rowCap, rows.length - shown.length)} more
        </button>
      )}
    </div>
  );
});
