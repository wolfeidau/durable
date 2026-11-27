import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, test } from "bun:test";
import {
  SandboxSnapshotSchema,
  SandboxStatus,
  SessionInfoSchema,
  SubmissionStatus,
  Workspace,
} from "../gen/durable/v1/durable_pb";
import {
  age,
  fastPollMs,
  kindLabel,
  kindText,
  normalPollMs,
  pollInterval,
  repoHref,
  sandboxHeaderNote,
  sandboxStatusView,
  sessionWarnings,
  showSandboxColumn,
  sorted,
  where,
  workStatus,
} from "./sessions";

const now = 1_700_000_000_000;

interface Init {
  id?: string;
  workspace?: Workspace;
  cwd?: string;
  repo?: string;
  environment?: string;
  branch?: string;
  pendingApprovals?: number;
  unfinishedSubmissions?: number;
  lastSubmissionStatus?: SubmissionStatus;
  sandboxStatus?: SandboxStatus;
  createdMs?: number;
}

function info({ createdMs, ...rest }: Init = {}) {
  return create(SessionInfoSchema, {
    ...rest,
    createdAt: createdMs === undefined ? undefined : timestampFromMs(createdMs),
  });
}

function snap(init: { observedMs?: number; refreshing?: boolean; stale?: boolean; error?: string } = {}) {
  const { observedMs, ...rest } = init;
  return create(SandboxSnapshotSchema, {
    ...rest,
    observedAt: observedMs === undefined ? undefined : timestampFromMs(observedMs),
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

describe("repoHref", () => {
  test("encodes each branch segment, keeping slashes", () => {
    const s = (branch: string) => info({ workspace: Workspace.REPO, repo: "github.com/o/r", branch });
    expect(repoHref(s("main"))).toBe("https://github.com/o/r/tree/main");
    expect(repoHref(s("feat/web"))).toBe("https://github.com/o/r/tree/feat/web");
    expect(repoHref(s("fix#12"))).toBe("https://github.com/o/r/tree/fix%2312");
  });
});

describe("pollInterval", () => {
  test("polls fast only while sandbox snapshot is refreshing", () => {
    expect(pollInterval(undefined)).toBe(normalPollMs);
    expect(pollInterval(snap({ refreshing: false }))).toBe(normalPollMs);
    expect(pollInterval(snap({ refreshing: true }))).toBe(fastPollMs);
  });
});

describe("workStatus", () => {
  test("covers all work states with approval taking precedence over working", () => {
    expect(
      workStatus(info({ pendingApprovals: 2, unfinishedSubmissions: 1, lastSubmissionStatus: SubmissionStatus.RUNNING })),
    ).toEqual({ text: "2 awaiting approval", className: "badge tone-warn" });
    expect(workStatus(info({ unfinishedSubmissions: 1, lastSubmissionStatus: SubmissionStatus.RUNNING }))).toEqual({
      text: "working",
      className: "badge tone-link pulse",
    });
    expect(workStatus(info({ lastSubmissionStatus: SubmissionStatus.FAILED }))).toEqual({
      text: "failed",
      className: "badge tone-error",
    });
    expect(workStatus(info({ lastSubmissionStatus: SubmissionStatus.CANCELLED }))).toEqual({
      text: "cancelled",
      className: "badge tone-cancelled",
    });
    expect(workStatus(info({ lastSubmissionStatus: SubmissionStatus.DONE }))).toEqual({
      text: "done",
      className: "muted",
    });
    expect(workStatus(info())).toEqual({ text: "new", className: "faint" });
  });
});

describe("showSandboxColumn", () => {
  test("hides for directory-only lists and shows when snapshot or sandbox status is present", () => {
    expect(showSandboxColumn([], undefined)).toBe(false);
    expect(showSandboxColumn([info({ sandboxStatus: SandboxStatus.UNSPECIFIED })], undefined)).toBe(false);
    expect(showSandboxColumn([], snap({ refreshing: true }))).toBe(true);
    expect(showSandboxColumn([info({ sandboxStatus: SandboxStatus.RUNNING })], undefined)).toBe(true);
  });
});

describe("sandboxHeaderNote", () => {
  test("reflects cold, updating, fresh, and stale cached states", () => {
    expect(sandboxHeaderNote(undefined, now)).toBeUndefined();
    expect(sandboxHeaderNote(snap({ refreshing: true, stale: true }), now)).toBe("checking...");
    expect(sandboxHeaderNote(snap({ refreshing: false, stale: true, error: "unavailable" }), now)).toBeUndefined();
    expect(sandboxHeaderNote(snap({ observedMs: now - 5_000, refreshing: false, stale: false }), now)).toBeUndefined();
    expect(sandboxHeaderNote(snap({ observedMs: now - 30_000, refreshing: true, stale: true }), now)).toBe("updating...");
    expect(sandboxHeaderNote(snap({ observedMs: now - 30_000, refreshing: false, stale: true }), now)).toBe("cached just now");
    expect(sandboxHeaderNote(snap({ observedMs: now - 180_000, refreshing: false, stale: true }), now)).toBe("cached 3m ago");
  });
});

describe("sandboxStatusView", () => {
  test("renders UNKNOWN neutrally as checking or unknown without stale class", () => {
    expect(sandboxStatusView(SandboxStatus.UNKNOWN, snap({ refreshing: true, stale: true }))).toEqual({
      text: "checking...",
      className: "faint",
      title: undefined,
    });
    expect(
      sandboxStatusView(SandboxStatus.UNKNOWN, snap({ refreshing: false, stale: true, error: "gateway unreachable" })),
    ).toEqual({
      text: "unknown",
      className: "faint",
      title: "gateway unreachable",
    });
  });

  test("renders all known states and applies stale only to known states", () => {
    const fresh = snap({ observedMs: now, stale: false });
    const stale = snap({ observedMs: now - 60_000, stale: true });
    const missingTitle = "Sandbox not found on gateway; a fresh one will be created when opened";

    expect(sandboxStatusView(SandboxStatus.RUNNING, fresh)).toEqual({ text: "running", className: "badge tone-ok" });
    expect(sandboxStatusView(SandboxStatus.RUNNING, stale)).toEqual({ text: "running", className: "badge tone-ok stale" });
    expect(sandboxStatusView(SandboxStatus.STARTING, fresh)).toEqual({ text: "starting", className: "badge tone-warn" });
    expect(sandboxStatusView(SandboxStatus.STARTING, stale)).toEqual({ text: "starting", className: "badge tone-warn stale" });
    expect(sandboxStatusView(SandboxStatus.STOPPING, fresh)).toEqual({ text: "stopping", className: "badge tone-muted" });
    expect(sandboxStatusView(SandboxStatus.STOPPING, stale)).toEqual({ text: "stopping", className: "badge tone-muted stale" });
    expect(sandboxStatusView(SandboxStatus.STOPPED, fresh)).toEqual({ text: "stopped", className: "faint" });
    expect(sandboxStatusView(SandboxStatus.STOPPED, stale)).toEqual({ text: "stopped", className: "faint stale" });
    expect(sandboxStatusView(SandboxStatus.ERROR, fresh)).toEqual({ text: "error", className: "badge tone-error" });
    expect(sandboxStatusView(SandboxStatus.ERROR, stale)).toEqual({ text: "error", className: "badge tone-error stale" });
    expect(sandboxStatusView(SandboxStatus.MISSING, fresh)).toEqual({ text: "absent", className: "faint", title: missingTitle });
    expect(sandboxStatusView(SandboxStatus.MISSING, stale)).toEqual({
      text: "absent",
      className: "faint stale",
      title: missingTitle,
    });
    expect(sandboxStatusView(SandboxStatus.UNSPECIFIED, stale)).toEqual({ text: "-", className: "faint" });
  });
});

describe("sessionWarnings", () => {
  test("appends non-empty snapshot error without duplicating existing warnings", () => {
    expect(sessionWarnings(undefined, undefined)).toEqual([]);
    expect(sessionWarnings(["bad pin"], snap({ error: "" }))).toEqual(["bad pin"]);
    expect(sessionWarnings(["bad pin"], snap({ error: "gateway down" }))).toEqual(["bad pin", "gateway down"]);
    expect(sessionWarnings(["gateway down"], snap({ error: "gateway down" }))).toEqual(["gateway down"]);
  });
});
