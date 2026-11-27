import { useMutation, useQuery } from "@connectrpc/connect-query";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { type ReactElement } from "react";
import { listSessions } from "../gen/durable/v1/durable-SessionService_connectquery";
import { type SandboxSnapshot, type SessionInfo } from "../gen/durable/v1/durable_pb";
import { QueryStatus } from "../ui/QueryStatus";
import { NewSessionForm } from "./NewSessionForm";
import {
  age,
  kindText,
  pollInterval,
  sandboxHeaderNote,
  sandboxStatusView,
  sessionWarnings,
  showSandboxColumn,
  sorted,
  where,
  workStatus,
} from "./sessions";

function Status({ s }: { s: SessionInfo }): ReactElement {
  const view = workStatus(s);
  return <span className={view.className}>{view.text}</span>;
}

function Sandbox({ s, snapshot }: { s: SessionInfo; snapshot: SandboxSnapshot | undefined }): ReactElement {
  const view = sandboxStatusView(s.sandboxStatus, snapshot);
  return (
    <span className={view.className} title={view.title}>
      {view.text}
    </span>
  );
}

export function SessionList({ onSelect }: { onSelect: (id: string) => void }): ReactElement {
  const { data, dataUpdatedAt, error, isPending, refetch } = useQuery(
    listSessions,
    {},
    { refetchInterval: (query) => pollInterval(query.state.data?.sandboxSnapshot) },
  );
  const refresh = useMutation(listSessions);
  const sessions = sorted(data?.sessions ?? []);
  const snapshot = data?.sandboxSnapshot;
  const hasSandbox = showSandboxColumn(sessions, snapshot);
  const headerNote = sandboxHeaderNote(snapshot);
  const observedTitle = snapshot?.observedAt ? `Updated ${timestampDate(snapshot.observedAt).toLocaleString()}` : undefined;
  const warnings = sessionWarnings(data?.warnings, snapshot);
  const refreshError = refresh.error && refresh.submittedAt >= dataUpdatedAt ? refresh.error : undefined;

  const onRefresh = async () => {
    const res = await refresh.mutateAsync({ refreshSandboxStatus: true }).catch(() => undefined);
    if (res) await refetch();
  };

  const newSession = () => {
    const form = document.getElementById("new-session");
    form?.scrollIntoView({ behavior: "smooth", block: "start" });
    form?.querySelector<HTMLElement>("input")?.focus({ preventScroll: true });
  };

  return (
    <div className="session-list">
      <div className="section-head">
        <h2>
          Sessions {data && <span className="muted count">{sessions.length}</span>}
        </h2>
        <div className="actions">
          <button className="secondary" disabled={refresh.isPending} onClick={() => void onRefresh()}>
            Refresh
          </button>
          <button onClick={newSession}>New session</button>
        </div>
      </div>
      <QueryStatus isPending={isPending} error={error ?? refreshError} warnings={warnings} />
      {data && sessions.length === 0 && <p className="card empty">No sessions yet. Create one below.</p>}
      {sessions.length > 0 && (
        <div className="table-wrap">
          <table className={hasSandbox ? "has-sandbox" : undefined}>
            <thead>
              <tr>
                <th>Name</th>
                <th>Workspace</th>
                <th>Status</th>
                {hasSandbox && (
                  <th className="sandbox-col" title={observedTitle}>
                    Sandbox{headerNote && <span className="sandbox-note">{headerNote}</span>}
                  </th>
                )}
                <th className="created">Created</th>
              </tr>
            </thead>
            <tbody>
              {sessions.map((s) => (
                <tr
                  key={s.id}
                  onClick={(e) => {
                    if (!(e.target as HTMLElement).closest("a")) onSelect(s.id);
                  }}
                >
                  <td>
                    <a
                      className="session-name"
                      href={`?session=${encodeURIComponent(s.id)}`}
                      onClick={(e) => {
                        e.preventDefault();
                        onSelect(s.id);
                      }}
                    >
                      {s.name || s.id}
                    </a>
                  </td>
                  <td className="workspace">
                    <span className="kind faint">{kindText(s)}</span>
                    <span className="where mono" title={where(s)}>
                      <bdi dir="ltr">{where(s)}</bdi>
                    </span>
                  </td>
                  <td>
                    <div className="status">
                      <Status s={s} />
                    </div>
                  </td>
                  {hasSandbox && (
                    <td className="sandbox">
                      <Sandbox s={s} snapshot={snapshot} />
                    </td>
                  )}
                  <td className="created muted" title={s.createdAt ? timestampDate(s.createdAt).toLocaleString() : undefined}>
                    {age(s)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <NewSessionForm
        onCreated={(id) => {
          void refetch();
          onSelect(id);
        }}
      />
    </div>
  );
}
