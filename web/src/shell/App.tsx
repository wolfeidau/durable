import { useEffect, useState, type ReactElement } from "react";
import { useRoute } from "../lib/route";
import { setToken } from "../lib/token";
import { SessionList } from "../sessions/SessionList";
import { SessionView } from "../session/SessionView";
import type { WatchSummary } from "../session/StatusLine";
import { ServerPage } from "../server/ServerPage";
import { onUnauthenticated, queryClient } from "./client";
import { TokenInput } from "./TokenInput";
import { Topbar } from "./Topbar";

export function App(): ReactElement {
  const [route, select, openServer, selectTab, selectFocus] = useRoute();
  const sessionId = route.view === "session" ? route.id : "";
  const onServer = route.view === "server";
  const [authNeeded, setAuthNeeded] = useState(false);
  const [watch, setWatch] = useState<WatchSummary | undefined>();

  useEffect(() => onUnauthenticated(() => setAuthNeeded(true)), []);

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
      <Topbar
        sessionId={sessionId}
        onServer={onServer}
        home={home}
        openServer={openServer}
        activeWatch={activeWatch}
        authNeeded={authNeeded}
      />
      {needToken && <TokenInput onApply={onToken} />}
      <main className={`page${sessionId ? " page-session" : ""}`}>
        {sessionId ? (
          <SessionView
            key={sessionId}
            sessionId={sessionId}
            onWatch={setWatch}
            tab={route.view === "session" ? route.tab : undefined}
            call={route.view === "session" ? route.call : undefined}
            onTab={(tab, call) => selectTab(sessionId, tab, call)}
            focus={route.view === "session" ? route.focus : undefined}
            onFocus={(target) => selectFocus(sessionId, target)}
          />
        ) : onServer ? (
          <ServerPage />
        ) : (
          <SessionList onSelect={select} />
        )}
      </main>
    </>
  );
}
