import { useCallback, useEffect, useState } from "react";

const read = () => new URLSearchParams(location.search).get("session") ?? "";

export function useSelectedSession(): [string, (id: string) => void] {
  const [id, setId] = useState(read);

  useEffect(() => {
    const onPop = () => setId(read());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  const select = useCallback((next: string) => {
    history.pushState(null, "", next ? `?session=${encodeURIComponent(next)}` : location.pathname);
    setId(next);
  }, []);

  return [id, select];
}
