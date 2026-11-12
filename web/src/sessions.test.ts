import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, test } from "bun:test";
import { SessionInfoSchema, Workspace } from "./gen/durable/v1/durable_pb";
import { age, kindLabel, kindText, sorted, where } from "./sessions";

const now = 1_700_000_000_000;

interface Init {
  id?: string;
  workspace?: Workspace;
  cwd?: string;
  repo?: string;
  environment?: string;
  branch?: string;
  pendingApprovals?: number;
  createdMs?: number;
}

function info({ createdMs, ...rest }: Init = {}) {
  return create(SessionInfoSchema, {
    ...rest,
    createdAt: createdMs === undefined ? undefined : timestampFromMs(createdMs),
  });
}

describe("age", () => {
  const ago = (secs: number) => age(info({ createdMs: now - secs * 1000 }), now);

  test("unit boundaries", () => {
    expect(ago(59)).toBe("just now");
    expect(ago(60)).toBe("1m ago");
    expect(ago(3599)).toBe("59m ago");
    expect(ago(3600)).toBe("1h ago");
    expect(ago(86400)).toBe("1d ago");
    expect(ago(86400 * 3)).toBe("3d ago");
  });

  test("a future timestamp reads as just now", () => {
    expect(ago(-30)).toBe("just now");
  });

  test("a missing createdAt is empty", () => {
    expect(age(info(), now)).toBe("");
  });
});

describe("workspace labels", () => {
  test("directory", () => {
    const s = info({ workspace: Workspace.DIRECTORY, cwd: "/work" });
    expect(where(s)).toBe("/work");
    expect(kindLabel(s)).toBe("Directory");
  });

  test("empty", () => {
    const s = info({ workspace: Workspace.EMPTY });
    expect(where(s)).toBe("empty workspace");
    expect(kindLabel(s)).toBe("Empty");
  });

  test("repo", () => {
    const s = info({ workspace: Workspace.REPO, repo: "github.com/o/n", branch: "main" });
    expect(where(s)).toBe("github.com/o/n@main");
    expect(kindLabel(s)).toBe("Repo");
  });
});

describe("kindText", () => {
  test("adds the environment when set", () => {
    expect(kindText(info({ workspace: Workspace.REPO, environment: "go" }))).toBe("Repo · go");
    expect(kindText(info({ workspace: Workspace.REPO }))).toBe("Repo");
  });
});

describe("sorted", () => {
  test("pending approvals first, then newest", () => {
    const old = info({ id: "old", createdMs: 1000 });
    const fresh = info({ id: "fresh", createdMs: 3000 });
    const waiting = info({ id: "waiting", createdMs: 500, pendingApprovals: 1 });
    expect(sorted([old, fresh, waiting]).map((s) => s.id)).toEqual(["waiting", "fresh", "old"]);
  });

  test("does not mutate its input", () => {
    const list = [info({ id: "a", createdMs: 1 }), info({ id: "b", createdMs: 2 })];
    sorted(list);
    expect(list.map((s) => s.id)).toEqual(["a", "b"]);
  });
});
