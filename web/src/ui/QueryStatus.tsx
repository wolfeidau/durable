import type { ReactElement } from "react";
import { errText } from "../lib/rpc";

export function QueryStatus({ isPending, error, warnings }: { isPending: boolean; error: unknown; warnings: string[] | undefined }): ReactElement {
  return (
    <>
      {isPending && !error && <p className="muted">Loading</p>}
      {!!error && <p className="banner tone-error">{errText(error)}</p>}
      {warnings?.map((w) => (
        <p className="banner tone-warn" key={w}>
          {w}
        </p>
      ))}
    </>
  );
}
