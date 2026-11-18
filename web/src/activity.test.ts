import { describe, expect, test } from "bun:test";
import { activeTurn, activityText } from "./activity";
import { SubmissionStatus } from "./gen/durable/v1/durable_pb";
import type { Turn } from "./transcript";

const call = { kind: "call" as const, id: "c", name: "bash", rewritten: false, isError: false };
const turn = (seq: bigint, status: SubmissionStatus, extra: Partial<Turn> = {}): Turn => ({ seq, status, entries: [], ...extra });

describe("activeTurn", () => {
  test("picks the newest queued or running turn", () => {
    const turns = [turn(1n, SubmissionStatus.RUNNING), turn(2n, SubmissionStatus.DONE), turn(3n, SubmissionStatus.QUEUED)];
    expect(activeTurn(turns)?.seq).toBe(3n);
    expect(activeTurn([turn(1n, SubmissionStatus.DONE)])).toBeUndefined();
    expect(activeTurn([])).toBeUndefined();
  });
});

describe("activityText", () => {
  test("running with elapsed time and calls", () => {
    const t = turn(1n, SubmissionStatus.RUNNING, { startedAt: 1000, entries: [call, call, call] });
    expect(activityText(t, 13000)).toBe("running · 12s · 3 calls");
  });

  test("singular call", () => {
    const t = turn(1n, SubmissionStatus.RUNNING, { startedAt: 1000, entries: [call] });
    expect(activityText(t, 3500)).toBe("running · 2.5s · 1 call");
  });

  test("leaves out unknown parts", () => {
    expect(activityText(turn(1n, SubmissionStatus.RUNNING), 5000)).toBe("running");
    expect(activityText(turn(1n, SubmissionStatus.QUEUED, { startedAt: 1 }), 5000)).toBe("queued");
  });
});
