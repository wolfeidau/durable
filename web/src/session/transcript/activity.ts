import { SubmissionStatus } from "../../gen/durable/v1/durable_pb";
import { duration, plural } from "../../lib/format";
import { isActive } from "../../lib/status";
import type { Turn } from "./transcript";

// The newest turn that is still queued or running.
export function activeTurn(turns: readonly Turn[]): Turn | undefined {
  for (let i = turns.length - 1; i >= 0; i--) if (isActive(turns[i]!.status)) return turns[i];
  return undefined;
}

// "running · 12s · 3 calls", leaving out what is unknown.
export function activityText(turn: Turn, now: number): string {
  const parts = [turn.status === SubmissionStatus.QUEUED ? "queued" : "running"];
  if (turn.status === SubmissionStatus.RUNNING && turn.startedAt !== undefined) parts.push(duration(now - turn.startedAt));
  const calls = turn.entries.filter((e) => e.kind === "call").length;
  if (calls > 0) parts.push(plural(calls, "call"));
  return parts.join(" · ");
}
