import { duration } from "./calls";
import { SubmissionStatus } from "./gen/durable/v1/durable_pb";
import type { Turn } from "./transcript";

export const isActive = (s: SubmissionStatus): boolean => s === SubmissionStatus.QUEUED || s === SubmissionStatus.RUNNING;

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
  if (calls > 0) parts.push(`${calls} ${calls === 1 ? "call" : "calls"}`);
  return parts.join(" · ");
}
