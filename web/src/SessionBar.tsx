import { useQuery } from "@connectrpc/connect-query";
import type { ReactElement } from "react";
import { getServerInfo, listSessions } from "./gen/durable/v1/durable-SessionService_connectquery";
import { Workspace } from "./gen/durable/v1/durable_pb";
import { environmentLabel } from "./newSession";
import { repoHref, where } from "./sessions";
import { useCopy } from "./useCopy";

// Workspace, environment and id of the open session, shown in the topbar.
export function SessionBar({ sessionId }: { sessionId: string }): ReactElement | null {
  const sessions = useQuery(listSessions, {});
  const info = useQuery(getServerInfo, {});
  const [copied, copy] = useCopy();
  const meta = sessions.data?.sessions.find((s) => s.id === sessionId);
  if (!meta) return null;
  const env = info.data?.environments.find((e) => e.name === meta.environment);
  const place = where(meta);
  return (
    <div className="session-bar">
      {meta.workspace === Workspace.REPO ? (
        <a
          className="mono bar-where"
          href={repoHref(meta)}
          target="_blank"
          rel="noopener noreferrer"
          title={place}
        >
          {place}
        </a>
      ) : (
        <span className="mono bar-where muted" title={place}>
          {place}
        </span>
      )}
      {meta.environment && <span className="badge tone-muted bar-env">{env ? environmentLabel(env) : meta.environment}</span>}
      <button className="bar-id mono faint" title={sessionId} aria-label="Copy session id" onClick={() => copy(sessionId)}>
        {copied ? "copied" : sessionId}
      </button>
    </div>
  );
}
