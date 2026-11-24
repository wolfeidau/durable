import { describe, expect, test } from "bun:test";
import { viewOf } from "./view";

describe("viewOf", () => {
  test("follows the tab when nothing is focused", () => {
    expect(viewOf(undefined, undefined)).toBe("transcript");
    expect(viewOf("report", undefined)).toBe("report");
    expect(viewOf("network", undefined)).toBe("network");
  });

  test("a focus target shows the transcript whatever the tab", () => {
    expect(viewOf("report", "turn-2")).toBe("transcript");
    expect(viewOf("network", "call-c1")).toBe("transcript");
    expect(viewOf(undefined, "call-c1")).toBe("transcript");
  });

  test("an empty focus is no focus", () => {
    expect(viewOf("report", "")).toBe("report");
  });
});
