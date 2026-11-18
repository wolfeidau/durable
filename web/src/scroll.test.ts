import { describe, expect, test } from "bun:test";
import { nearBottom } from "./scroll";

describe("nearBottom", () => {
  test("at the bottom and within the threshold", () => {
    expect(nearBottom(600, 400, 1000)).toBe(true);
    expect(nearBottom(520, 400, 1000)).toBe(true);
  });

  test("scrolled up", () => {
    expect(nearBottom(519, 400, 1000)).toBe(false);
    expect(nearBottom(0, 400, 1000)).toBe(false);
  });

  test("content shorter than the viewport", () => {
    expect(nearBottom(0, 800, 500)).toBe(true);
  });
});
