import { useCallback, useEffect, useState } from "react";

export type Route = { view: "list" } | { view: "server" } | { view: "session"; id: string; tab?: "report" | "network"; call?: string; focus?: string };

const focusPattern = /^(call|turn)-.+$/;
const callPattern = /^[A-Za-z0-9_.:-]+$/;

export type Tab = "report" | "network";

export function parseRoute(search: string): Route {
  const params = new URLSearchParams(search);
  const id = params.get("session");
  if (id) {
    const focus = params.get("focus");
    const tab = params.get("tab");
    const call = params.get("call");
    return {
      view: "session",
      id,
      ...(tab === "report" || tab === "network" ? { tab } : {}),
      ...(tab === "network" && call && callPattern.test(call) ? { call } : {}),
      ...(focus && focusPattern.test(focus) ? { focus } : {}),
    };
  }
  if (params.get("view") === "server") return { view: "server" };
  return { view: "list" };
}

const read = () => parseRoute(location.search);

export const serverHref = "?view=server";

export function useRoute(): [
  Route,
  (id: string) => void,
  () => void,
  (id: string, tab: Tab | undefined, call?: string) => void,
  (id: string, focus: string | undefined) => void,
] {
  const [route, setRoute] = useState(read);

  useEffect(() => {
    const onPop = () => setRoute(read());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  const select = useCallback((next: string) => {
    history.pushState(null, "", next ? `?session=${encodeURIComponent(next)}` : location.pathname);
    setRoute(read());
  }, []);

  const openServer = useCallback(() => {
    history.pushState(null, "", serverHref);
    setRoute(read());
  }, []);

  const selectTab = useCallback((id: string, tab: Tab | undefined, call?: string) => {
    const callParam = tab === "network" && call ? `&call=${encodeURIComponent(call)}` : "";
    history.replaceState(null, "", `?session=${encodeURIComponent(id)}${tab ? `&tab=${tab}` : ""}${callParam}`);
    setRoute(read());
  }, []);

  // The query string, not the hash, which the token login reads.
  const selectFocus = useCallback((id: string, focus: string | undefined) => {
    history.replaceState(null, "", `?session=${encodeURIComponent(id)}${focus ? `&focus=${encodeURIComponent(focus)}` : ""}`);
    setRoute(read());
  }, []);

  return [route, select, openServer, selectTab, selectFocus];
}
