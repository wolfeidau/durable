import { describe, expect, test } from "bun:test";
import { Workspace } from "./gen/durable/v1/durable_pb";
import { buildRequest, canCreate, emptyOnly, RequestIds } from "./newSession";

describe("new session request", () => {
  test("empty mode sends the empty workspace and a request id, without a cwd", () => {
    expect(buildRequest({ cwd: "/ignored", name: " n " }, true, "req-1")).toEqual({
      cwd: "",
      name: "n",
      workspace: Workspace.EMPTY,
      requestId: "req-1",
    });
  });

  test("directory mode is unchanged", () => {
    expect(buildRequest({ cwd: " /work ", name: "" }, false, "req-1")).toEqual({
      cwd: "/work",
      name: "",
      workspace: Workspace.DIRECTORY,
      requestId: "",
    });
  });

  test("empty mode needs no cwd, directory mode does", () => {
    expect(canCreate({ cwd: " ", name: "" }, true)).toBe(true);
    expect(canCreate({ cwd: " ", name: "" }, false)).toBe(false);
    expect(canCreate({ cwd: "/a", name: "" }, false)).toBe(true);
  });

  test("the server's kinds pick the mode", () => {
    expect(emptyOnly([Workspace.EMPTY])).toBe(true);
    expect(emptyOnly([Workspace.DIRECTORY])).toBe(false);
    expect(emptyOnly([])).toBe(false);
  });
});

describe("request ids", () => {
  test("the id is reused after a failure and fresh after a success", () => {
    let n = 0;
    const ids = new RequestIds(() => `id-${++n}`);
    expect(ids.get()).toBe("id-1");
    expect(ids.get()).toBe("id-1");
    ids.succeeded();
    expect(ids.get()).toBe("id-2");
  });
});

test("the default id is 32 random hex characters", () => {
  const ids = new RequestIds();
  expect(ids.get()).toMatch(/^[0-9a-f]{32}$/);
  const first = ids.get();
  ids.succeeded();
  expect(ids.get()).not.toBe(first);
});
