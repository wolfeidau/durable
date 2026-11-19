import { useEffect, useState, useSyncExternalStore, type DependencyList } from "react";

interface Engine<S> {
  start: () => void;
  stop: () => void;
  subscribe: (listener: () => void) => () => void;
  getSnapshot: () => S;
}

const noop = () => {};
const noSubscribe = () => noop;

// `deps` is passed explicitly so a caller can leave out values read on use, like a token getter.
export function useEngine<S, E extends Engine<S>>(create: () => E | undefined, deps: DependencyList, fallback: () => S): { engine: E | undefined; state: S } {
  const [engine, setEngine] = useState<E | undefined>();

  useEffect(() => {
    const e = create();
    setEngine(e);
    if (!e) return;
    e.start();
    return () => e.stop();
  }, deps);

  const state = useSyncExternalStore(engine?.subscribe ?? noSubscribe, engine?.getSnapshot ?? fallback);
  return { engine, state };
}
