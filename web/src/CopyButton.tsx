import type { ReactElement } from "react";
import { useCopy } from "./useCopy";

export function CopyButton({ text }: { text: string }): ReactElement {
  const [copied, copy] = useCopy();
  return (
    <button className="secondary copy-btn" aria-label="Copy message" onClick={() => copy(text)}>
      {copied ? "Copied" : "Copy"}
    </button>
  );
}
