import { createContext, useContext, useLayoutEffect, useMemo, useRef, useState, type FocusEvent, type PointerEvent, type ReactElement, type ReactNode } from "react";

interface Tip {
  x: number;
  y: number;
  content: ReactNode;
}

interface TipApi {
  show: (x: number, y: number, content: ReactNode) => void;
  hide: () => void;
}

const TipContext = createContext<TipApi>({ show: () => {}, hide: () => {} });

export const useTip = (): TipApi => useContext(TipContext);

const gap = 14;

// State lives here, so moving the pointer re-renders the tooltip and not the charts passed as children.
export function TipHost({ children }: { children: ReactNode }): ReactElement {
  const [tip, setTip] = useState<Tip>();
  const box = useRef<HTMLDivElement>(null);
  const api = useMemo<TipApi>(() => ({ show: (x, y, content) => setTip({ x, y, content }), hide: () => setTip(undefined) }), []);

  useLayoutEffect(() => {
    const el = box.current;
    if (!el || !tip) return;
    const { width, height } = el.getBoundingClientRect();
    const left = tip.x + gap + width > innerWidth ? tip.x - gap - width : tip.x + gap;
    const top = tip.y + gap + height > innerHeight ? tip.y - gap - height : tip.y + gap;
    el.style.left = `${Math.max(4, left)}px`;
    el.style.top = `${Math.max(4, top)}px`;
  }, [tip]);

  return (
    <TipContext.Provider value={api}>
      {children}
      {tip && (
        <div className="tip" role="tooltip" ref={box}>
          {tip.content}
        </div>
      )}
    </TipContext.Provider>
  );
}

// Handlers that show the same content on pointer hover and keyboard focus.
export function tipProps(api: TipApi, content: ReactNode) {
  return {
    onPointerMove: (e: PointerEvent) => api.show(e.clientX, e.clientY, content),
    onPointerLeave: api.hide,
    onFocus: (e: FocusEvent<HTMLElement>) => {
      const r = e.currentTarget.getBoundingClientRect();
      api.show(r.left + Math.min(r.width, 240) / 2, r.top + r.height / 2, content);
    },
    onBlur: api.hide,
  };
}
