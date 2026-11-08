import { Code, ConnectError, type Interceptor } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { createConnectTransport } from "@connectrpc/connect-web";
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useEffect, useState, type ReactElement } from "react";
import { createRoot } from "react-dom/client";
import { SessionList } from "./SessionList";
import { SessionView } from "./SessionView";
import { StatusLine } from "./StatusLine";
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

function App(): ReactElement {
  const [sessionId, select] = useSelectedSession();
  const [authNeeded, setAuthNeeded] = useState(false);

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
  };

  return (
    <div className="app">
      <header>
        <h1>
          <a
            href={location.pathname}
            onClick={(e) => {
              e.preventDefault();
              select("");
            }}
          >
            durable
          </a>
        </h1>
        {sessionId && <span className="muted session-id">{sessionId}</span>}
      </header>
      <main>
        {sessionId ? (
          <SessionView key={sessionId} sessionId={sessionId} authNeeded={authNeeded} onToken={onToken} onBack={() => select("")} />
        ) : (
          <>
            <StatusLine authNeeded={authNeeded} onToken={onToken} />
            <SessionList onSelect={select} />
          </>
        )}
      </main>
    </div>
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
