import { describe, expect, test } from "bun:test";
import { plural } from "./format";

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
