import { describe, expect, test } from "bun:test";
import { parseRoute } from "./route";

describe("route", () => {
  test("no query is the list", () => {
    expect(parseRoute("")).toEqual({ view: "list" });
  });

  test("view=server is the server page", () => {
    expect(parseRoute("?view=server")).toEqual({ view: "server" });
  });

  test("a session wins over a view", () => {
    expect(parseRoute("?session=s-1&view=server")).toEqual({ view: "session", id: "s-1" });
  });

  test("an unknown view is the list", () => {
    expect(parseRoute("?view=other")).toEqual({ view: "list" });
  });
});
