import { useQuery } from "@connectrpc/connect-query";
import type { ReactElement } from "react";
import { listSessions } from "../gen/durable/v1/durable-SessionService_connectquery";
import { serverHref } from "../lib/route";
import { ConnectionPill, type WatchSummary } from "../session/StatusLine";

function Crumb({ sessionId }: { sessionId: string }): ReactElement {
  const { data } = useQuery(listSessions, {});
  const name = data?.sessions.find((s) => s.id === sessionId)?.name;
  return <span className="crumb-name" aria-current="page">{name || sessionId}</span>;
}

interface Props {
  sessionId: string;
  onServer: boolean;
  home: (e: { preventDefault: () => void }) => void;
  openServer: () => void;
  activeWatch?: WatchSummary;
  authNeeded: boolean;
}

export function Topbar({ sessionId, onServer, home, openServer, activeWatch, authNeeded }: Props): ReactElement {
  return (
    <header className="topbar">
      <nav className="crumbs" aria-label="Breadcrumb">
        <a className="brand" href={location.pathname} onClick={home}>
          durable
        </a>
        {sessionId && (
          <>
            <span className="sep crumb-mid" aria-hidden="true">
              /
            </span>
            <a className="crumb-mid" href={location.pathname} onClick={home}>
              Sessions
            </a>
            <span className="sep" aria-hidden="true">
              /
            </span>
            <Crumb sessionId={sessionId} />
          </>
        )}
        {onServer && (
          <>
            <span className="sep" aria-hidden="true">
              /
            </span>
            <span className="crumb-name" aria-current="page">
              Server
            </span>
          </>
        )}
      </nav>
      <div className="topbar-right">
        <a
          className="nav-link"
          href={serverHref}
          aria-current={onServer ? "page" : undefined}
          onClick={(e) => {
            e.preventDefault();
            openServer();
          }}
        >
          Server
        </a>
        <ConnectionPill watch={activeWatch} authNeeded={authNeeded} />
      </div>
    </header>
  );
}
