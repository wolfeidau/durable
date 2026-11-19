import { useState, type ReactElement } from "react";
import type { WatchHandle } from "./useWatchEvents";

export type WatchSummary = Pick<WatchHandle, "status" | "lastError" | "resumeAuth">;

export function TokenInput({ onApply }: { onApply: (t: string) => void }): ReactElement {
  const [value, setValue] = useState("");
  return (
    <form
      className="banner tone-warn token-box"
      onSubmit={(e) => {
        e.preventDefault();
        onApply(value.trim());
      }}
    >
      <label>
        Token needed
        <input type="password" autoComplete="off" value={value} onChange={(e) => setValue(e.target.value)} />
      </label>
      <button type="submit">Apply</button>
    </form>
  );
}

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
