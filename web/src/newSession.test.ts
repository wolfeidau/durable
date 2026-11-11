import { describe, expect, test } from "bun:test";
import { Workspace } from "./gen/durable/v1/durable_pb";
import { buildRequest, canCreate, defaultBranch, modesFor, RequestIds } from "./newSession";

const base = { mode: "directory" as const, cwd: "", name: "", repo: "", branch: defaultBranch };

describe("new session request", () => {
  test("empty mode sends the empty workspace and a request id, without a cwd", () => {
    expect(buildRequest({ ...base, mode: "empty", cwd: "/ignored", name: " n " }, "req-1")).toEqual({
      cwd: "",
      name: "n",
      workspace: Workspace.EMPTY,
      requestId: "req-1",
      repo: "",
      branch: "",
    });
  });

  test("directory mode sends only the cwd and name", () => {
    expect(buildRequest({ ...base, cwd: " /work ", repo: "ignored" }, "req-1")).toEqual({
      cwd: "/work",
      name: "",
      workspace: Workspace.DIRECTORY,
      requestId: "",
      repo: "",
      branch: "",
    });
  });

  test("repo mode sends the repo and branch with no request id", () => {
    const form = { ...base, mode: "repo" as const, repo: " github.com/o/n ", branch: " dev ", name: "x" };
    expect(buildRequest(form, "req-1")).toEqual({
      cwd: "",
      name: "x",
      workspace: Workspace.REPO,
      requestId: "",
      repo: "github.com/o/n",
      branch: "dev",
    });
  });

  test("the branch defaults to main", () => {
    expect(defaultBranch).toBe("main");
  });

  test("each mode needs its own fields", () => {
    expect(canCreate({ ...base, mode: "empty" })).toBe(true);
    expect(canCreate({ ...base, cwd: " " })).toBe(false);
    expect(canCreate({ ...base, cwd: "/a" })).toBe(true);
    expect(canCreate({ ...base, mode: "repo", repo: "github.com/o/n" })).toBe(true);
    expect(canCreate({ ...base, mode: "repo", repo: " " })).toBe(false);
    expect(canCreate({ ...base, mode: "repo", repo: "github.com/o/n", branch: " " })).toBe(false);
  });

  test("the server's kinds pick the modes", () => {
    expect(modesFor([Workspace.EMPTY, Workspace.REPO])).toEqual(["empty", "repo"]);
    expect(modesFor([Workspace.DIRECTORY])).toEqual(["directory"]);
    expect(modesFor([])).toEqual([]);
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
