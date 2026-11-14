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

  test("tab=report opens the report on a session", () => {
    expect(parseRoute("?session=s-1&tab=report")).toEqual({ view: "session", id: "s-1", tab: "report" });
    expect(parseRoute("?session=s-1&tab=other")).toEqual({ view: "session", id: "s-1" });
    expect(parseRoute("?tab=report")).toEqual({ view: "list" });
  });

  test("an unknown view is the list", () => {
    expect(parseRoute("?view=other")).toEqual({ view: "list" });
  });
});
