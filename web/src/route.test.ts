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

  test("focus targets a call or turn in the transcript", () => {
    expect(parseRoute("?session=s-1&focus=call-abc")).toEqual({ view: "session", id: "s-1", focus: "call-abc" });
    expect(parseRoute("?session=s-1&tab=report&focus=turn-3")).toEqual({ view: "session", id: "s-1", tab: "report", focus: "turn-3" });
    expect(parseRoute("?session=s-1&focus=other-1")).toEqual({ view: "session", id: "s-1" });
    expect(parseRoute("?session=s-1&focus=call-")).toEqual({ view: "session", id: "s-1" });
    expect(parseRoute("?focus=call-abc")).toEqual({ view: "list" });
  });

  test("an unknown view is the list", () => {
    expect(parseRoute("?view=other")).toEqual({ view: "list" });
  });

  test("tab=network carries an optional call", () => {
    expect(parseRoute("?session=s-1&tab=network")).toEqual({ view: "session", id: "s-1", tab: "network" });
    expect(parseRoute("?session=s-1&tab=network&call=toolu_01:a.b-c")).toEqual({
      view: "session",
      id: "s-1",
      tab: "network",
      call: "toolu_01:a.b-c",
    });
    expect(parseRoute("?session=s-1&tab=network&call=")).toEqual({ view: "session", id: "s-1", tab: "network" });
    expect(parseRoute("?session=s-1&tab=network&call=a%20b")).toEqual({ view: "session", id: "s-1", tab: "network" });
    expect(parseRoute("?session=s-1&tab=report&call=abc")).toEqual({ view: "session", id: "s-1", tab: "report" });
  });
});
