import type { ReactElement } from "react";
import type { WatchHandle } from "./useWatchEvents";

export type WatchSummary = Pick<WatchHandle, "status" | "lastError" | "resumeAuth">;

function describe(w: WatchSummary): { text: string; tone: string } {
  switch (w.status) {
    case "live":
      return { text: "live", tone: "tone-ok" };
    case "connecting":
      return { text: "connecting", tone: "tone-warn" };
    case "reconnecting":
      return { text: w.lastError ? `reconnecting: ${w.lastError}` : "reconnecting", tone: "tone-warn" };
    case "auth-needed":
      return { text: "token needed", tone: "tone-error" };
    case "not-found":
      return { text: "session not found", tone: "tone-error" };
    default:
      return { text: "stopped", tone: "tone-muted" };
  }
}

interface Props {
  watch?: WatchSummary;
  authNeeded: boolean;
}

export function ConnectionPill({ watch, authNeeded }: Props): ReactElement | null {
  const needToken = authNeeded || watch?.status === "auth-needed";
  if (!watch && !needToken) return null;
  const s = needToken || !watch ? { text: "token needed", tone: "tone-error" } : describe(watch);
  return (
    <span className={`pill ${s.tone}`} role="status" title={s.text}>
      <span className="dot" aria-hidden="true" />
      <span className="pill-text">{s.text}</span>
    </span>
  );
}
