import { Code, ConnectError, type Interceptor } from "@connectrpc/connect";
import { TransportProvider, useQuery } from "@connectrpc/connect-query";
import { createConnectTransport } from "@connectrpc/connect-web";
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useEffect, useState, type ReactElement } from "react";
import { createRoot } from "react-dom/client";
import { listSessions } from "./gen/durable/v1/durable-SessionService_connectquery";
import { SessionList } from "./SessionList";
import { SessionView } from "./SessionView";
import { ConnectionPill, TokenInput, type WatchSummary } from "./StatusLine";
import { getToken, setToken } from "./token";
import { useSelectedSession } from "./useSelectedSession";
import "./app.css";

const bearer: Interceptor = (next) => (req) => {
  req.header.set("Authorization", `Bearer ${getToken()}`);
  return next(req);
};
const transport = createConnectTransport({ baseUrl: location.origin, interceptors: [bearer] });

// The client lives outside React, so the auth flag goes through a listener.
let onUnauthenticated = () => {};
const flagAuth = (err: unknown) => {
  if (ConnectError.from(err).code === Code.Unauthenticated) onUnauthenticated();
};
const queryClient = new QueryClient({
  queryCache: new QueryCache({ onError: flagAuth }),
  mutationCache: new MutationCache({ onError: flagAuth }),
});

function Crumb({ sessionId }: { sessionId: string }): ReactElement {
  const { data } = useQuery(listSessions, {});
  const name = data?.sessions.find((s) => s.id === sessionId)?.name;
  return <span className="crumb-name">{name || sessionId}</span>;
}

function App(): ReactElement {
  const [sessionId, select] = useSelectedSession();
  const [authNeeded, setAuthNeeded] = useState(false);
  const [watch, setWatch] = useState<WatchSummary | undefined>();

  useEffect(() => {
    onUnauthenticated = () => setAuthNeeded(true);
    return () => {
      onUnauthenticated = () => {};
    };
  }, []);

  const onToken = (t: string) => {
    setToken(t);
    setAuthNeeded(false);
    void queryClient.refetchQueries();
    watch?.resumeAuth();
  };

  const home = (e: { preventDefault: () => void }) => {
    e.preventDefault();
    select("");
  };
  const activeWatch = sessionId ? watch : undefined;
  const needToken = authNeeded || activeWatch?.status === "auth-needed";

  return (
    <>
      <header className="topbar">
        <nav className="crumbs" aria-label="Breadcrumb">
          <a className="brand" href={location.pathname} onClick={home}>
            durable
          </a>
          {sessionId && (
            <>
              <span className="sep" aria-hidden="true">
                /
              </span>
              <a href={location.pathname} onClick={home}>
                Sessions
              </a>
              <span className="sep" aria-hidden="true">
                /
              </span>
              <Crumb sessionId={sessionId} />
            </>
          )}
        </nav>
        <ConnectionPill watch={activeWatch} authNeeded={authNeeded} />
      </header>
      {needToken && <TokenInput onApply={onToken} />}
      <main className={`page${sessionId ? " page-session" : ""}`}>
        {sessionId ? (
          <SessionView key={sessionId} sessionId={sessionId} onWatch={setWatch} />
        ) : (
          <SessionList onSelect={select} />
        )}
      </main>
    </>
  );
}

const root = document.getElementById("root");
if (root) {
  createRoot(root).render(
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>
    </TransportProvider>,
  );
}
