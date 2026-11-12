import { describe, expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import { EnvironmentInfoSchema, Workspace } from "./gen/durable/v1/durable_pb";
import { buildRequest, canCreate, defaultBranch, environmentHint, modesFor, pickEnvironment, RequestIds } from "./newSession";

const base = { mode: "directory" as const, cwd: "", name: "", repo: "", branch: defaultBranch, environment: "go" };

describe("new session request", () => {
  test("empty mode sends the empty workspace and a request id, without a cwd", () => {
    expect(buildRequest({ ...base, mode: "empty", cwd: "/ignored", name: " n " }, "req-1")).toEqual({
      cwd: "",
      name: "n",
      workspace: Workspace.EMPTY,
      requestId: "req-1",
      repo: "",
      branch: "",
      environment: "go",
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
      environment: "",
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
      environment: "go",
    });
  });

  test("the environment is sent in empty and repo mode but never for a directory", () => {
    expect(buildRequest({ ...base, mode: "empty" }, "r").environment).toBe("go");
    expect(buildRequest({ ...base, mode: "repo", repo: "a", branch: "b" }, "r").environment).toBe("go");
    expect(buildRequest({ ...base, cwd: "/w" }, "r").environment).toBe("");
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

describe("pickEnvironment", () => {
  test("keeps the chosen name while it is listed", () => {
    expect(pickEnvironment(["default", "go"], "go")).toBe("go");
  });

  test("prefers default when the choice is missing or unlisted", () => {
    expect(pickEnvironment(["go", "default"])).toBe("default");
    expect(pickEnvironment(["go", "default"], "gone")).toBe("default");
  });

  test("falls back to the first, or empty with none", () => {
    expect(pickEnvironment(["a", "b"])).toBe("a");
    expect(pickEnvironment([])).toBe("");
  });
});

describe("environmentHint", () => {
  test("lists the image and grants", () => {
    const e = create(EnvironmentInfoSchema, { name: "go", image: "durable-worker-go:dev", grants: ["go-modules"] });
    expect(environmentHint(e)).toBe("durable-worker-go:dev · go-modules");
  });

  test("says so when there are no grants", () => {
    const e = create(EnvironmentInfoSchema, { name: "default", image: "durable-worker:dev" });
    expect(environmentHint(e)).toBe("durable-worker:dev · no network grants");
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
