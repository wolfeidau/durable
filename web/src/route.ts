import { useCallback, useEffect, useState } from "react";

export type Route = { view: "list" } | { view: "server" } | { view: "session"; id: string };

export function parseRoute(search: string): Route {
  const params = new URLSearchParams(search);
  const id = params.get("session");
  if (id) return { view: "session", id };
  if (params.get("view") === "server") return { view: "server" };
  return { view: "list" };
}

const read = () => parseRoute(location.search);

export const serverHref = "?view=server";

export function useRoute(): [Route, (id: string) => void, () => void] {
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

  return [route, select, openServer];
}
