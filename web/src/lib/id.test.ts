import { describe, expect, test } from "bun:test";
import { randomId } from "./id";

describe("randomId", () => {
  test("returns a 32-character lowercase hex string", () => {
    const a = randomId();
    const b = randomId();
    expect(a).toMatch(/^[0-9a-f]{32}$/);
    expect(b).toMatch(/^[0-9a-f]{32}$/);
    expect(a).not.toBe(b);
  });
});
