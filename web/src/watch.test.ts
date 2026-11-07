import { describe, expect, test } from "bun:test";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import type { MessageInitShape } from "@bufbuild/protobuf";
import {
  DeltaKind,
  EpochMismatchSchema,
  EventKind,
  Role,
  SessionService,
  SubmissionStatus,
  type WatchEventsResponseSchema,
} from "./gen/durable/v1/durable_pb";
import { WatchEngine, type WatchState } from "./watch";

type Item = MessageInitShape<typeof WatchEventsResponseSchema>;
// Each attempt streams its items, then ends the way `end` says; "hang" waits for the client to abort.
interface Attempt {
  items?: Item[];
  end?: "hang" | "close" | ConnectError;
}

interface Seen {
  epoch: string;
  afterSeq: bigint;
  includeDeltas: boolean;
  auth: string | null;
}

const ev = (seq: number, extra: Record<string, unknown> = {}): Item => ({
  item: { case: "event", value: { seq: BigInt(seq), epoch: "e1", kind: EventKind.SUBMISSION, ...extra } },
});
const hb: Item = { item: { case: "heartbeat", value: {} } };
const delta = (kind: DeltaKind, extra: Record<string, unknown> = {}): Item => ({
  item: { case: "delta", value: { lastSeq: 0n, kind, submission: 1n, ...extra } },
});
const assistant = (seq: number, submission = 1n): Item =>
  ev(seq, { kind: EventKind.MESSAGE, submission, message: { role: Role.ASSISTANT, text: "done" } });

function setup(attempts: Attempt[], token = () => "tok") {
  const seen: Seen[] = [];
  const sleeps: number[] = [];
  const transport = createRouterTransport(({ service }) => {
    service(SessionService, {
      async *watchEvents(req, ctx) {
        seen.push({
          epoch: req.epoch,
          afterSeq: req.afterSeq,
          includeDeltas: req.includeDeltas,
          auth: ctx.requestHeader.get("Authorization"),
        });
        const a = attempts[seen.length - 1] ?? { end: "hang" };
        yield* a.items ?? [];
        if (a.end instanceof ConnectError) throw a.end;
        if (a.end === "close") return;
        await new Promise<void>((resolve) => {
          if (ctx.signal.aborted) resolve();
          ctx.signal.addEventListener("abort", () => resolve());
        });
      },
    });
  });
  const engine = new WatchEngine({
    transport,
    sessionId: "s1",
    getToken: token,
    sleep: async (ms) => {
      sleeps.push(ms);
    },
  });
  return { engine, seen, sleeps };
}

async function until(engine: WatchEngine, pred: (s: WatchState) => boolean): Promise<WatchState> {
  for (let i = 0; i < 500; i++) {
    const s = engine.getSnapshot();
    if (pred(s)) return s;
    await new Promise((resolve) => setTimeout(resolve, 2));
  }
  throw new Error(`condition not met: ${JSON.stringify(engine.getSnapshot(), (_, v) => (typeof v === "bigint" ? String(v) : v))}`);
}

const mismatch = () =>
  new ConnectError("epoch changed", Code.FailedPrecondition, undefined, [
    { desc: EpochMismatchSchema, value: { epoch: "e2", lastSeq: 1n } },
  ]);

describe("WatchEngine", () => {
  test("applies events in order and watches with deltas from the cursor", async () => {
    const { engine, seen } = setup([{ items: [ev(1), ev(2), ev(3)] }]);
    engine.start();
    const s = await until(engine, (s) => s.cursor === 3n);
    expect(s.status).toBe("live");
    expect(s.epoch).toBe("e1");
    expect(s.events.map((e) => e.seq)).toEqual([1n, 2n, 3n]);
    expect(s.gaps).toBe(0);
    expect(seen[0]).toEqual({ epoch: "", afterSeq: 0n, includeDeltas: true, auth: "Bearer tok" });
    engine.stop();
  });

  test("drops duplicates and counts a gap but still applies", async () => {
    const { engine } = setup([{ items: [ev(1), ev(1), ev(2), ev(2), ev(5), ev(4)] }]);
    engine.start();
    const s = await until(engine, (s) => s.cursor === 5n);
    await new Promise((resolve) => setTimeout(resolve, 10));
    const after = engine.getSnapshot();
    expect(s.events.map((e) => e.seq)).toEqual([1n, 2n, 5n]);
    expect(after.events.map((e) => e.seq)).toEqual([1n, 2n, 5n]);
    expect(after.gaps).toBe(1);
    engine.stop();
  });

  test("snapshots are immutable and change by reference", async () => {
    const { engine } = setup([{ items: [ev(1)] }]);
    const first = engine.getSnapshot();
    let calls = 0;
    engine.subscribe(() => calls++);
    engine.start();
    const s = await until(engine, (s) => s.cursor === 1n);
    expect(s).not.toBe(first);
    expect(first.events).toEqual([]);
    expect(calls).toBeGreaterThan(0);
    expect(engine.getSnapshot()).toBe(s);
    engine.stop();
  });

  test("epoch mismatch clears state and replays from epoch empty and seq 0", async () => {
    const { engine, seen } = setup([
      { items: [ev(1), delta(DeltaKind.RESET), delta(DeltaKind.TEXT, { text: "x" })], end: mismatch() },
      { items: [ev(1, { epoch: "e2" })] },
    ]);
    engine.start();
    const s = await until(engine, (s) => s.epoch === "e2");
    expect(seen[1]).toMatchObject({ epoch: "", afterSeq: 0n });
    expect(s.events.map((e) => e.epoch)).toEqual(["e2"]);
    expect(s.partialText.size).toBe(0);
    expect(s.reconnects).toBe(0);
    engine.stop();
  });

  test("a reset forgets which submissions were reset", async () => {
    const { engine } = setup([
      { items: [delta(DeltaKind.RESET)], end: mismatch() },
      { items: [delta(DeltaKind.TEXT, { text: "frag" }), hb] },
    ]);
    engine.start();
    const s = await until(engine, (s) => s.lastHeartbeat !== undefined);
    expect(s.partialText.size).toBe(0);
    engine.stop();
  });

  test("waits on unauthenticated until resumeAuth, then reconnects from the cursor", async () => {
    const { engine, seen, sleeps } = setup([
      { items: [ev(1)], end: new ConnectError("no", Code.Unauthenticated) },
      { items: [ev(2)] },
    ]);
    engine.start();
    await until(engine, (s) => s.status === "auth-needed");
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(seen.length).toBe(1);
    engine.resumeAuth();
    const s = await until(engine, (s) => s.cursor === 2n);
    expect(seen[1]).toMatchObject({ epoch: "e1", afterSeq: 1n });
    expect(sleeps).toEqual([]);
    expect(s.reconnects).toBe(0);
    engine.stop();
  });

  test("setToken resumes with the new token", async () => {
    const { engine, seen } = setup([{ end: new ConnectError("no", Code.Unauthenticated) }, { items: [ev(1)] }]);
    engine.start();
    await until(engine, (s) => s.status === "auth-needed");
    engine.setToken("fresh");
    await until(engine, (s) => s.cursor === 1n);
    expect(seen.map((s) => s.auth)).toEqual(["Bearer tok", "Bearer fresh"]);
    engine.stop();
  });

  test("backs off 1s, 2s, 4s up to a 10s cap and resets after an item", async () => {
    const unavailable = new ConnectError("down", Code.Unavailable);
    const { engine, sleeps, seen } = setup([
      { end: unavailable },
      { end: "close" },
      { end: unavailable },
      { end: unavailable },
      { end: unavailable },
      { end: unavailable },
      { items: [ev(1)], end: unavailable },
      { items: [ev(2)] },
    ]);
    engine.start();
    const s = await until(engine, (s) => s.cursor === 2n);
    expect(sleeps).toEqual([1000, 2000, 4000, 8000, 10_000, 10_000, 1000]);
    expect(s.reconnects).toBe(7);
    expect(seen[7]).toMatchObject({ epoch: "e1", afterSeq: 1n });
    engine.stop();
  });

  test("heartbeat updates lastHeartbeat", async () => {
    const { engine } = setup([{ items: [hb] }]);
    engine.start();
    const s = await until(engine, (s) => s.lastHeartbeat !== undefined);
    expect(s.status).toBe("live");
    engine.stop();
  });

  test("reset then text appends, and an assistant message clears it", async () => {
    const { engine } = setup([
      {
        items: [
          delta(DeltaKind.RESET),
          delta(DeltaKind.TEXT, { text: "he" }),
          delta(DeltaKind.TEXT, { text: "llo" }),
        ],
      },
    ]);
    engine.start();
    const s = await until(engine, (s) => s.partialText.get(1n) === "hello");
    expect(s.partialText.size).toBe(1);
    engine.stop();

    const b = setup([{ items: [delta(DeltaKind.RESET), delta(DeltaKind.TEXT, { text: "hi" }), assistant(1)] }]);
    b.engine.start();
    const done = await until(b.engine, (s) => s.cursor === 1n);
    expect(done.partialText.size).toBe(0);
    b.engine.stop();
  });

  test("text before any reset is skipped until the submission resets", async () => {
    const { engine } = setup([
      {
        items: [
          delta(DeltaKind.TEXT, { text: "frag" }),
          delta(DeltaKind.RESET),
          delta(DeltaKind.TEXT, { text: "ok" }),
          delta(DeltaKind.TEXT, { submission: 2n, text: "other" }),
          hb,
        ],
      },
    ]);
    engine.start();
    const s = await until(engine, (s) => s.lastHeartbeat !== undefined);
    expect([...s.partialText]).toEqual([[1n, "ok"]]);
    engine.stop();
  });

  test("a terminal submission status clears its partial text", async () => {
    const { engine } = setup([
      {
        items: [
          delta(DeltaKind.RESET),
          delta(DeltaKind.TEXT, { text: "x" }),
          delta(DeltaKind.RESET, { submission: 2n }),
          ev(1, { submission: 1n, status: SubmissionStatus.FAILED }),
        ],
      },
    ]);
    engine.start();
    const s = await until(engine, (s) => s.cursor === 1n);
    expect([...s.partialText.keys()]).toEqual([2n]);
    engine.stop();
  });

  test("tool output accumulates per call and call_done clears only that call", async () => {
    const out = (callId: string, text: string) => delta(DeltaKind.TOOL_OUTPUT, { callId, text });
    const { engine } = setup([
      {
        items: [
          out("a", "1"),
          out("b", "x"),
          out("a", "2"),
          ev(1, { kind: EventKind.CALL_DONE, call: { id: "a", name: "bash" } }),
        ],
      },
    ]);
    engine.start();
    const s = await until(engine, (s) => s.cursor === 1n);
    expect([...s.toolOutput]).toEqual([["b", "x"]]);
    engine.stop();
  });

  test("tool output builds up before call_done", async () => {
    const { engine } = setup([
      { items: [delta(DeltaKind.TOOL_OUTPUT, { callId: "a", text: "1" }), delta(DeltaKind.TOOL_OUTPUT, { callId: "a", text: "2" })] },
    ]);
    engine.start();
    const s = await until(engine, (s) => s.toolOutput.get("a") === "12");
    expect(s.toolOutput.size).toBe(1);
    engine.stop();
  });

  test("stop ends the loop without reconnecting", async () => {
    const { engine, seen, sleeps } = setup([{ items: [ev(1)] }]);
    engine.start();
    await until(engine, (s) => s.cursor === 1n);
    engine.stop();
    expect(engine.getSnapshot().status).toBe("stopped");
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(seen.length).toBe(1);
    expect(sleeps).toEqual([]);
    expect(engine.getSnapshot().status).toBe("stopped");
    expect(engine.getSnapshot().reconnects).toBe(0);
  });

  test("stop during the backoff sleep does not reconnect", async () => {
    const { engine, seen } = setup([{ end: new ConnectError("down", Code.Unavailable) }]);
    // Never resolves, so only stop can end the wait.
    (engine as unknown as { sleep: () => Promise<void> }).sleep = () => new Promise(() => {});
    engine.start();
    await until(engine, (s) => s.status === "reconnecting");
    engine.stop();
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(seen.length).toBe(1);
    expect(engine.getSnapshot().status).toBe("stopped");
  });

  test("stop while waiting for auth does not reconnect", async () => {
    const { engine, seen } = setup([{ end: new ConnectError("no", Code.Unauthenticated) }]);
    engine.start();
    await until(engine, (s) => s.status === "auth-needed");
    engine.stop();
    engine.resumeAuth();
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(seen.length).toBe(1);
    expect(engine.getSnapshot().status).toBe("stopped");
  });
});

describe("WatchEngine review fixes", () => {
  test("drops live text from a previous connection", async () => {
    const { engine } = setup([
      { items: [ev(1), delta(DeltaKind.RESET), delta(DeltaKind.TEXT, { text: "ab" })], end: "close" },
      { items: [delta(DeltaKind.TEXT, { text: "cd" }), ev(2)], end: "hang" },
    ]);
    engine.start();
    const s = await until(engine, (st) => st.cursor === 2n);
    expect(s.partialText.has(1n)).toBe(false);
    engine.stop();
  });

  test("publishes a long replay in few snapshots", async () => {
    const items = Array.from({ length: 2000 }, (_, i) => ev(i + 1));
    const { engine } = setup([{ items, end: "hang" }]);
    let notified = 0;
    engine.subscribe(() => notified++);
    engine.start();
    const s = await until(engine, (st) => st.cursor === 2000n);
    expect(s.events.length).toBe(2000);
    expect(notified).toBeLessThan(2000);
    engine.stop();
  });
});
