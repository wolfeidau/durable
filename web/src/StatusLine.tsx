import { useState, type ReactElement } from "react";
import type { WatchHandle } from "./useWatchEvents";

export function TokenInput({ onApply }: { onApply: (t: string) => void }): ReactElement {
  const [value, setValue] = useState("");
  return (
    <form
      className="token-box"
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

function describe(w: WatchHandle): { text: string; tone: string } {
  switch (w.status) {
    case "live":
      return { text: "live", tone: "ok" };
    case "connecting":
      return { text: "connecting", tone: "warn" };
    case "reconnecting":
      return { text: w.lastError ? `reconnecting: ${w.lastError}` : "reconnecting", tone: "warn" };
    case "auth-needed":
      return { text: "token needed", tone: "bad" };
    default:
      return { text: "stopped", tone: "muted" };
  }
}

interface Props {
  watch?: WatchHandle;
  authNeeded: boolean;
  onToken: (t: string) => void;
}

export function StatusLine({ watch, authNeeded, onToken }: Props): ReactElement | null {
  const needToken = authNeeded || watch?.status === "auth-needed";
  if (!watch && !needToken) return null;
  const s = watch ? describe(watch) : { text: "token needed", tone: "bad" };
  return (
    <div className="status-line">
      <span className={`dot ${s.tone}`} aria-hidden="true" />
      <span>{needToken ? "token needed" : s.text}</span>
      {needToken && (
        <TokenInput
          onApply={(t) => {
            onToken(t);
            watch?.resumeAuth();
          }}
        />
      )}
    </div>
  );
}
