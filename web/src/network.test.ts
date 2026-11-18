import { describe, expect, test } from "bun:test";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { create } from "@bufbuild/protobuf";
import { timestampDate, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import {
  ListNetworkEventsResponseSchema,
  NetworkCoverageSchema,
  NetworkEventSchema,
  NetworkSource,
  SessionService,
  type ListNetworkEventsResponse,
  type NetworkCoverage,
  type NetworkEvent,
} from "./gen/durable/v1/durable_pb";
import {
  coverageNotes,
  deniedByCall,
  gatewayIntervalMs,
  maxEvents,
  merge,
  NetworkPoller,
  networkSourceLabel,
  storeIntervalMs,
  type NetworkState,
} from "./network";

const since = new Date(1_000_000);
const ev = (id: string, t: number, allowed = true): NetworkEvent =>
  create(NetworkEventSchema, { id, allowed, time: timestampFromMs(t) });
const cov = (c: MessageInitShape<typeof NetworkCoverageSchema> = {}): NetworkCoverage =>
  create(NetworkCoverageSchema, { source: NetworkSource.STORE, resumable: true, storeAvailable: true, ...c });
const res = (r: MessageInitShape<typeof ListNetworkEventsResponseSchema> = {}): ListNetworkEventsResponse =>
  create(ListNetworkEventsResponseSchema, { coverage: cov(), ...r });
const base = (extra: Partial<NetworkState> = {}): NetworkState => ({
  status: "loading",
  events: [],
  olderTruncated: false,
  capped: false,
  since,
  ...extra,
});
const ids = (s: NetworkState) => s.events.map((e) => e.id);

describe("merge", () => {
  test("first load replaces and records truncation", () => {
    const prev = base({ events: [ev("old", 1)] });
    const s = merge(prev, res({ events: [ev("b", 2), ev("a", 1)], truncated: true }), true);
    expect(ids(s)).toEqual(["a", "b"]);
    expect(s.olderTruncated).toBe(true);
  });

  test("appends, dedupes with the new event winning, and sorts out-of-order arrivals", () => {
    const first = merge(base(), res({ events: [ev("a", 10), ev("c", 30)] }), true);
    const s = merge(first, res({ events: [ev("b", 20), ev("c", 30, false)], truncated: true }), false);
    expect(ids(s)).toEqual(["a", "b", "c"]);
    expect(s.events[2]!.allowed).toBe(false);
    expect(s.olderTruncated).toBe(false);
  });

  test("every gateway-log poll updates truncation", () => {
    const gwCov = cov({ source: NetworkSource.GATEWAY_LOG, resumable: false });
    const first = merge(base(), res({ events: [ev("a", 1)], coverage: gwCov }), true);
    expect(first.olderTruncated).toBe(false);
    const s = merge(first, res({ events: [ev("b", 2)], coverage: gwCov, truncated: true }), false);
    expect(s.olderTruncated).toBe(true);
    expect(merge(s, res({ events: [ev("b", 2)], coverage: gwCov }), false).olderTruncated).toBe(false);
  });

  test("ties on time order by id", () => {
    const s = merge(base(), res({ events: [ev("b", 5), ev("a", 5)] }), true);
    expect(ids(s)).toEqual(["a", "b"]);
  });

  test("cursorReset replaces", () => {
    const first = merge(base(), res({ events: [ev("a", 1)] }), true);
    const s = merge(first, res({ events: [ev("z", 9)], cursorReset: true, truncated: true }), false);
    expect(ids(s)).toEqual(["z"]);
    expect(s.olderTruncated).toBe(true);
  });

  test("a source change replaces", () => {
    const first = merge(base(), res({ events: [ev("a", 1)] }), true);
    const gw = res({ events: [ev("h1", 2)], coverage: cov({ source: NetworkSource.GATEWAY_LOG, resumable: false }) });
    expect(ids(merge(first, gw, false))).toEqual(["h1"]);
  });

  test("the gateway source merges by id", () => {
    const gw = cov({ source: NetworkSource.GATEWAY_LOG, resumable: false });
    const first = merge(base(), res({ events: [ev("h1", 1)], coverage: gw }), true);
    const s = merge(first, res({ events: [ev("h1", 1), ev("h2", 2)], coverage: gw }), false);
    expect(ids(s)).toEqual(["h1", "h2"]);
  });

  test("caps at the newest maxEvents and stays capped", () => {
    const many = Array.from({ length: maxEvents + 5 }, (_, i) => ev(`e${String(i).padStart(6, "0")}`, i));
    const s = merge(base(), res({ events: many }), true);
    expect(s.events.length).toBe(maxEvents);
    expect(s.events[0]!.id).toBe("e000005");
    expect(s.capped).toBe(true);
    expect(merge(s, res({ events: [ev("x", 1e9)] }), false).capped).toBe(true);
  });
});

type Handler = (req: { after: string; sinceSeconds: bigint }, n: number) => ListNetworkEventsResponse | ConnectError;

function setup(handler: Handler, opts: { hidden?: () => boolean } = {}) {
  const seen: { after: string; sinceSeconds: bigint }[] = [];
  const sleeps: number[] = [];
  const transport = createRouterTransport(({ service }) => {
    service(SessionService, {
      async listNetworkEvents(req) {
        const r = { after: req.after, sinceSeconds: req.since.case === "sinceTime" ? req.since.value.seconds : -1n };
        seen.push(r);
        const out = handler(r, seen.length);
        if (out instanceof ConnectError) throw out;
        return out;
      },
    });
  });
  // Each sleep waits for the test to release it, so polls advance one step at a time.
  const gate: (() => void)[] = [];
  const sleep = (ms: number) => {
    sleeps.push(ms);
    return new Promise<void>((resolve) => gate.push(resolve));
  };
  const poller = new NetworkPoller({ transport, sessionId: "s-1", since, sleep, hidden: opts.hidden });
  const tick = () => new Promise<void>((resolve) => setTimeout(resolve, 20));
  const next = async () => {
    await tick();
    gate.shift()?.();
    await tick();
  };
  return { poller, seen, sleeps, tick, next };
}

describe("NetworkPoller", () => {
  test("sends since_time and resumes from the cursor", async () => {
    const { poller, seen, sleeps, tick, next } = setup((_, n) => res({ events: [ev(`e${n}`, n)], nextCursor: `c${n}` }));
    poller.start();
    await tick();
    expect(seen).toEqual([{ after: "", sinceSeconds: 1000n }]);
    expect(poller.getSnapshot().status).toBe("live");
    expect(sleeps).toEqual([storeIntervalMs]);
    await next();
    expect(seen[1]!.after).toBe("c1");
    expect(ids(poller.getSnapshot())).toEqual(["e1", "e2"]);
    poller.stop();
  });

  test("sends no cursor for the gateway source and waits longer", async () => {
    const gw = cov({ source: NetworkSource.GATEWAY_LOG, resumable: false });
    const { poller, seen, sleeps, tick, next } = setup(() => res({ coverage: gw, nextCursor: "ignored", events: [ev("h", 1)] }));
    poller.start();
    await tick();
    await next();
    expect(seen.map((s) => s.after)).toEqual(["", ""]);
    expect(sleeps[0]).toBe(gatewayIntervalMs);
    poller.stop();
  });

  test("polls again immediately when a resumed poll is truncated", async () => {
    const { poller, seen, sleeps, tick, next } = setup((_, n) =>
      res({ events: [ev(`e${n}`, n)], nextCursor: `c${n}`, truncated: n === 2 }),
    );
    poller.start();
    await tick();
    await next(); // second request is truncated and resumed
    await tick();
    expect(seen.length).toBe(3);
    expect(sleeps.length).toBe(2);
    poller.stop();
  });

  test("a truncated first load still sleeps", async () => {
    const { poller, seen, tick } = setup(() => res({ truncated: true, nextCursor: "c" }));
    poller.start();
    await tick();
    expect(seen.length).toBe(1);
    poller.stop();
  });

  test("FailedPrecondition means no sandbox and stops polling", async () => {
    const { poller, seen, sleeps, tick, next } = setup(() => new ConnectError("no sandbox", Code.FailedPrecondition));
    poller.start();
    await tick();
    await next();
    expect(poller.getSnapshot().status).toBe("no-sandbox");
    expect(seen.length).toBe(1);
    expect(sleeps).toEqual([]);
  });

  test("NotFound sets the error and stops", async () => {
    const { poller, seen, tick, next } = setup(() => new ConnectError("gone", Code.NotFound));
    poller.start();
    await tick();
    await next();
    expect(poller.getSnapshot().error).toBe("gone");
    expect(seen.length).toBe(1);
  });

  test("other errors keep the events, set the error and retry; success clears it", async () => {
    const { poller, seen, tick, next } = setup((_, n) =>
      n === 2 ? new ConnectError("boom", Code.Unavailable) : res({ events: [ev(`e${n}`, n)], nextCursor: `c${n}` }),
    );
    poller.start();
    await tick();
    await next();
    expect(poller.getSnapshot().error).toBe("boom");
    expect(ids(poller.getSnapshot())).toEqual(["e1"]);
    await next();
    expect(seen.length).toBe(3);
    expect(seen[2]!.after).toBe("c1");
    expect(poller.getSnapshot().error).toBeUndefined();
    expect(ids(poller.getSnapshot())).toEqual(["e1", "e3"]);
    poller.stop();
  });

  test("a hidden page skips requests", async () => {
    let hidden = true;
    const { poller, seen, sleeps, tick, next } = setup(() => res(), { hidden: () => hidden });
    poller.start();
    await tick();
    expect(seen.length).toBe(0);
    expect(sleeps).toEqual([storeIntervalMs]);
    hidden = false;
    await next();
    expect(seen.length).toBe(1);
    poller.stop();
  });

  test("stop ends the loop", async () => {
    const { poller, seen, tick, next } = setup(() => res());
    poller.start();
    await tick();
    poller.stop();
    await next();
    expect(poller.getSnapshot().status).toBe("stopped");
    expect(seen.length).toBe(1);
  });
});

describe("deniedByCall", () => {
  const events = [ev("a", 100, false), ev("b", 200, false), ev("c", 150, true), ev("d", 300, false)];

  test("assigns denials within the call window, inclusive", () => {
    const m = deniedByCall(events, [{ id: "k1", startedAt: 100, doneAt: 200 }], 1000);
    expect(m.get("k1")!.map((e) => e.id)).toEqual(["a", "b"]);
  });

  test("a running call extends to now", () => {
    const m = deniedByCall(events, [{ id: "k1", startedAt: 150 }], 250);
    expect(m.get("k1")!.map((e) => e.id)).toEqual(["b"]);
  });

  test("overlapping calls each get the event; calls without denials are absent", () => {
    const m = deniedByCall(
      events,
      [
        { id: "k1", startedAt: 0, doneAt: 250 },
        { id: "k2", startedAt: 190, doneAt: 400 },
        { id: "k3", startedAt: 120, doneAt: 160 },
      ],
      1000,
    );
    expect(m.get("k1")!.map((e) => e.id)).toEqual(["a", "b"]);
    expect(m.get("k2")!.map((e) => e.id)).toEqual(["b", "d"]);
    expect(m.has("k3")).toBe(false);
  });

  test("a call without startedAt is ignored", () => {
    expect(deniedByCall(events, [{ id: "k1" }], 1000).size).toBe(0);
  });
});

describe("coverageNotes", () => {
  const fmt = (d: Date) => `T${d.getTime()}`;
  const notes = (c: MessageInitShape<typeof NetworkCoverageSchema>, extra: Partial<NetworkState> = {}) =>
    coverageNotes(base({ coverage: cov(c), ...extra }), fmt);
  const t = (n: number) => timestampFromMs(n);

  test("clean coverage has no notes", () => {
    expect(notes({})).toEqual([]);
    expect(coverageNotes(base(), fmt)).toEqual([]);
  });

  test("gateway log source", () => {
    const n = notes({ source: NetworkSource.GATEWAY_LOG, sourceBounded: true, resumable: false, gatewayLogStart: t(5) });
    expect(n).toHaveLength(1);
    expect(n[0]!.text).toContain("T5");
    expect(n[0]!.text).toContain("earlier decisions are missing");
  });

  test("gateway error is an error", () => {
    expect(notes({ gatewayError: "x" })[0]!.tone).toBe("error");
  });

  test("history start and pruned", () => {
    const n = notes({ historyStart: t(7), pruned: true });
    expect(n.map((x) => x.text).join("\n")).toContain("nothing before T7");
    expect(n.map((x) => x.text).join("\n")).toContain("retention");
  });

  test("drops", () => {
    const n = notes({
      droppedOwn: { dropped: 3n, since: t(1) },
      droppedGatewayWide: { dropped: 1n, since: t(2) },
      droppedUnknown: { dropped: 2n, since: t(3) },
    });
    expect(n.map((x) => x.tone)).toEqual(["error", "warn", "warn"]);
    expect(n[0]!.text).toContain("3 events from this sandbox");
    expect(n[0]!.text).toContain("T1");
    expect(n[1]!.text).toContain("1 event were dropped gateway-wide");
    expect(n[2]!.text).toContain("may include this session");
  });

  test("unattributed", () => {
    const n = notes({ unattributed: 4n, unattributedFromDay: "2026-01-02" });
    expect(n[0]!.text).toContain("4 decisions");
    expect(n[0]!.text).toContain("2026-01-02");
  });

  test("client-side truncation", () => {
    const n = coverageNotes(base({ coverage: cov(), olderTruncated: true, capped: true }), fmt);
    expect(n).toHaveLength(2);
    expect(n[1]!.text).toContain("10,000");
  });

  test("store off suppresses store-derived notes", () => {
    const n = notes({
      storeAvailable: false,
      historyStart: t(1),
      droppedOwn: { dropped: 3n, since: t(1) },
      unattributed: 2n,
      pruned: true,
    });
    expect(n).toHaveLength(1);
    expect(n[0]!.text).toContain("store is off");
  });
});

test("networkSourceLabel", () => {
  expect(networkSourceLabel[NetworkSource.STORE]).toBe("telemetry");
  expect(networkSourceLabel[NetworkSource.GATEWAY_LOG]).toBe("gateway log");
  expect(timestampDate(timestampFromMs(5)).getTime()).toBe(5);
});

describe("merge keeps identity", () => {
  test("an empty poll returns the same events array", () => {
    const first = merge(
      { status: "loading", events: [], olderTruncated: false, capped: false, since: new Date(0) },
      create(ListNetworkEventsResponseSchema, { events: [create(NetworkEventSchema, { id: "a" })] }),
      true,
    );
    const next = merge(first, create(ListNetworkEventsResponseSchema, {}), false);
    expect(next.events).toBe(first.events);
  });
});
