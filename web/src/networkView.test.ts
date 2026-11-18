import { describe, expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { NetworkEventSchema, type NetworkEvent } from "./gen/durable/v1/durable_pb";
import { hostName, hostSummary, matches, stableDenied, topDenied } from "./networkView";

const ev = (id: string, t: number, o: Partial<Pick<NetworkEvent, "allowed" | "host" | "path" | "pattern" | "binary">> = {}): NetworkEvent =>
  create(NetworkEventSchema, { id, time: timestampFromMs(t), allowed: false, host: "a.com", ...o });

describe("topDenied", () => {
  test("groups by host name and pattern, skips allowed, orders by count", () => {
    const events = [
      ev("1", 1, { host: "a.com", pattern: "/v*/x" }),
      ev("2", 2, { host: "a.com", pattern: "/v*/x" }),
      ev("3", 3, { host: "b.com" }),
      ev("4", 4, { host: "b.com:443", binary: "/bin/curl" }),
      ev("5", 5, { host: "b.com:443", binary: "/bin/wget" }),
      ev("6", 6, { host: "c.com", allowed: true }),
      ev("7", 7, { host: "a.com", pattern: "/other" }),
    ];
    expect(topDenied(events)).toEqual([
      { host: "b.com", what: "", count: 3 },
      { host: "a.com", what: "/v*/x", count: 2 },
      { host: "a.com", what: "/other", count: 1 },
    ]);
  });

  test("hostName strips only a trailing port", () => {
    expect(hostName("a.com:443")).toBe("a.com");
    expect(hostName("a.com")).toBe("a.com");
    expect(hostName("[::1]:443")).toBe("[::1]");
    expect(hostName("::1")).toBe("::1");
  });

  test("keeps only the top rows", () => {
    const events = Array.from({ length: 12 }, (_, i) => ev(String(i), i, { host: `h${i}.com` }));
    expect(topDenied(events)).toHaveLength(8);
    expect(topDenied(events, 3)).toHaveLength(3);
  });
});

describe("matches", () => {
  const allowed = ev("a", 100, { allowed: true, host: "Api.Example.com", path: "/Repos" });
  const denied = ev("d", 200, { host: "evil.com", path: "/x" });
  test("verdict", () => {
    expect(matches(allowed, { verdict: "all", text: "" })).toBe(true);
    expect(matches(allowed, { verdict: "denied", text: "" })).toBe(false);
    expect(matches(denied, { verdict: "denied", text: "" })).toBe(true);
    expect(matches(denied, { verdict: "allowed", text: "" })).toBe(false);
  });
  test("text is a case-insensitive substring of host and path", () => {
    expect(matches(allowed, { verdict: "all", text: "example" })).toBe(true);
    expect(matches(allowed, { verdict: "all", text: " REPOS " })).toBe(true);
    expect(matches(allowed, { verdict: "all", text: "evil" })).toBe(false);
  });
  test("call window is inclusive", () => {
    const f = { verdict: "all" as const, text: "", window: { start: 100, end: 150 } };
    expect(matches(allowed, f)).toBe(true);
    expect(matches(denied, f)).toBe(false);
    expect(matches(denied, { ...f, window: { start: 150, end: 200 } })).toBe(true);
  });
});

describe("stableDenied", () => {
  const a = ev("a", 1);
  const b = ev("b", 2);
  test("reuses the whole map when nothing changed", () => {
    const prev = new Map([["c1", [a]]]);
    expect(stableDenied(prev, new Map([["c1", [a]]]))).toBe(prev);
  });
  test("keeps unchanged entries while replacing changed ones", () => {
    const prev = new Map([
      ["c1", [a]],
      ["c2", [a]],
    ]);
    const out = stableDenied(
      prev,
      new Map([
        ["c1", [a]],
        ["c2", [a, b]],
      ]),
    );
    expect(out).not.toBe(prev);
    expect(out.get("c1")).toBe(prev.get("c1")!);
    expect(out.get("c2")).toHaveLength(2);
  });
  test("a removed call changes the map", () => {
    const prev = new Map([
      ["c1", [a]],
      ["c2", [a]],
    ]);
    expect(stableDenied(prev, new Map([["c1", [a]]])).size).toBe(1);
  });
});

describe("hostSummary", () => {
  test("lists distinct hosts and counts the rest", () => {
    const events = ["a", "b", "a", "c", "d", "e", "f", "g"].map((h, i) => ev(String(i), i, { host: h }));
    expect(hostSummary(events)).toBe("a, b, c, d, e +2 more");
    expect(hostSummary([ev("x", 1, { host: "" })])).toBe("unknown host");
  });
});
