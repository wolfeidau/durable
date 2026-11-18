import { describe, expect, test } from "bun:test";
import { compact, duration, money, percent, plural, seconds } from "./format";

describe("plural", () => {
  test("numbers", () => {
    expect(plural(0, "call")).toBe("0 calls");
    expect(plural(1, "call")).toBe("1 call");
    expect(plural(3, "call")).toBe("3 calls");
  });

  test("bigints", () => {
    expect(plural(0n, "event")).toBe("0 events");
    expect(plural(1n, "event")).toBe("1 event");
    expect(plural(2n, "event")).toBe("2 events");
  });
});

describe("format", () => {
  test("compact numbers", () => {
    expect(compact(420_000)).toBe("420K");
    expect(compact(1_000_000)).toBe("1M");
    expect(compact(1_500_000)).toBe("1.5M");
    expect(compact(999)).toBe("999");
  });

  test("money", () => {
    expect(money(0.0123)).toBe("$0.0123");
    expect(money(0.004, true)).toBe("~$0.0040");
    expect(money(undefined)).toBe("—");
    expect(money(12.345)).toBe("$12.35");
  });
});

describe("percent", () => {
  test("never shows a small share as 0%", () => {
    expect(percent(undefined)).toBe("—");
    expect(percent(0)).toBe("0%");
    expect(percent(0.0037)).toBe("<1%");
    expect(percent(0.005)).toBe("1%");
    expect(percent(0.42)).toBe("42%");
  });
});

describe("duration", () => {
  test("formats by size", () => {
    expect(duration(0)).toBe("0ms");
    expect(duration(33)).toBe("33ms");
    expect(duration(999.6)).toBe("1.0s");
    expect(duration(1000)).toBe("1.0s");
    expect(duration(2140)).toBe("2.1s");
    expect(duration(12400)).toBe("12s");
    expect(duration(65000)).toBe("1m 05s");
    expect(duration(-5)).toBe("0ms");
  });
});

describe("seconds", () => {
  test("uses the duration format, with a dash when unknown", () => {
    expect(seconds(undefined)).toBe("—");
    expect(seconds(2140)).toBe("2.1s");
  });
});
