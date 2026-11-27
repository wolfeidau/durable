import { describe, expect, test } from "bun:test";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  ListSessionsResponseSchema,
  SandboxSnapshotSchema,
  SandboxStatus,
  SessionInfoSchema,
  SessionService,
  SubmissionStatus,
  Workspace,
  type ListSessionsRequest,
  type ListSessionsResponse,
} from "../gen/durable/v1/durable_pb";
import { SessionList } from "./SessionList";

interface RenderOpts {
  workspaces?: Workspace[];
  listSessions?: (req: ListSessionsRequest) => MessageInitShape<typeof ListSessionsResponseSchema> | ListSessionsResponse;
}

function renderList({ workspaces = [Workspace.DIRECTORY], listSessions }: RenderOpts = {}) {
  const transport = createRouterTransport(({ service }) => {
    service(SessionService, {
      listSessions: (req) =>
        listSessions ? listSessions(req) : create(ListSessionsResponseSchema, { sessions: [], warnings: [] }),
      getServerInfo: () => ({ settings: [], warnings: [], workspaces, environments: [], grants: [] }),
    });
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <TransportProvider transport={transport}>
      <QueryClientProvider client={qc}>
        <SessionList onSelect={() => {}} />
      </QueryClientProvider>
    </TransportProvider>,
  );
  return { ...view, qc };
}

function sessionRow(name: string): HTMLTableRowElement {
  const link = screen.getByRole("link", { name });
  const row = link.closest("tr");
  expect(row).not.toBeNull();
  return row!;
}

describe("NewSessionForm workspace caption", () => {
  test("omits the static caption when directory is the only workspace", async () => {
    const { container } = renderList({ workspaces: [Workspace.DIRECTORY] });
    await waitFor(() => expect(screen.getByLabelText("Working directory")).toBeTruthy());
    expect(container.querySelector("#new-session .kind")).toBeNull();
  });

  test("shows Repository when repo is the only workspace", async () => {
    const { container } = renderList({ workspaces: [Workspace.REPO] });
    await waitFor(() => expect(screen.getByLabelText("Repository")).toBeTruthy());
    expect(container.querySelector("#new-session .kind")?.textContent).toBe("Repository");
  });

  test("shows Empty workspace when empty is the only workspace", async () => {
    const { container } = renderList({ workspaces: [Workspace.EMPTY] });
    await waitFor(() => expect(screen.getByRole("button", { name: "Create" })).toBeTruthy());
    expect(container.querySelector("#new-session .kind")?.textContent).toBe("Empty workspace");
  });
});

describe("SessionList work and sandbox status", () => {
  test("renders all work statuses and omits Sandbox column for directory-only lists", async () => {
    const base = Date.now();
    const { container } = renderList({
      listSessions: () => ({
        warnings: [],
        sessions: [
          create(SessionInfoSchema, {
            id: "s-approval",
            name: "needs-approval",
            workspace: Workspace.DIRECTORY,
            cwd: "/work/a",
            pendingApprovals: 2,
            unfinishedSubmissions: 1,
            lastSubmissionStatus: SubmissionStatus.RUNNING,
            createdAt: timestampFromMs(base - 60_000),
          }),
          create(SessionInfoSchema, {
            id: "s-working",
            name: "active-work",
            workspace: Workspace.DIRECTORY,
            cwd: "/work/b",
            unfinishedSubmissions: 1,
            lastSubmissionStatus: SubmissionStatus.RUNNING,
            createdAt: timestampFromMs(base - 120_000),
          }),
          create(SessionInfoSchema, {
            id: "s-failed",
            name: "failed-work",
            workspace: Workspace.DIRECTORY,
            cwd: "/work/c",
            lastSubmissionStatus: SubmissionStatus.FAILED,
            createdAt: timestampFromMs(base - 180_000),
          }),
          create(SessionInfoSchema, {
            id: "s-cancelled",
            name: "cancelled-work",
            workspace: Workspace.DIRECTORY,
            cwd: "/work/d",
            lastSubmissionStatus: SubmissionStatus.CANCELLED,
            createdAt: timestampFromMs(base - 240_000),
          }),
          create(SessionInfoSchema, {
            id: "s-done",
            name: "done-work",
            workspace: Workspace.DIRECTORY,
            cwd: "/work/e",
            lastSubmissionStatus: SubmissionStatus.DONE,
            createdAt: timestampFromMs(base - 300_000),
          }),
          create(SessionInfoSchema, {
            id: "s-new",
            name: "fresh-session",
            workspace: Workspace.DIRECTORY,
            cwd: "/work/f",
            lastSubmissionStatus: SubmissionStatus.UNSPECIFIED,
            createdAt: timestampFromMs(base - 360_000),
          }),
        ],
      }),
    });

    await waitFor(() => expect(screen.getByRole("link", { name: "needs-approval" })).toBeTruthy());

    const headers = Array.from(container.querySelectorAll("thead th")).map((th) => th.textContent?.trim());
    expect(headers).toEqual(["Name", "Workspace", "Status", "Created"]);

    const approvalRow = sessionRow("needs-approval");
    expect(within(approvalRow).getByText("2 awaiting approval").className).toBe("badge tone-warn");
    expect(within(approvalRow).queryByText("working")).toBeNull();

    expect(within(sessionRow("active-work")).getByText("working").className).toBe("badge tone-link pulse");
    expect(within(sessionRow("failed-work")).getByText("failed").className).toBe("badge tone-error");
    expect(within(sessionRow("cancelled-work")).getByText("cancelled").className).toBe("badge tone-cancelled");
    expect(within(sessionRow("done-work")).getByText("done").className).toBe("muted");
    expect(within(sessionRow("fresh-session")).getByText("new").className).toBe("faint");
  });

  test("renders cold refreshing sandbox status as neutral checking...", async () => {
    const { container } = renderList({
      workspaces: [Workspace.EMPTY, Workspace.REPO],
      listSessions: () => ({
        warnings: [],
        sandboxSnapshot: create(SandboxSnapshotSchema, { refreshing: true, stale: true }),
        sessions: [
          create(SessionInfoSchema, {
            id: "sb-cold",
            name: "cold-sandbox",
            workspace: Workspace.EMPTY,
            sandboxStatus: SandboxStatus.UNKNOWN,
          }),
        ],
      }),
    });

    await waitFor(() => expect(screen.getByRole("link", { name: "cold-sandbox" })).toBeTruthy());

    const sandboxHeader = container.querySelector("thead th.sandbox-col");
    expect(sandboxHeader?.textContent).toBe("Sandboxchecking...");
    expect(sandboxHeader?.getAttribute("title")).toBeNull();

    const cell = sessionRow("cold-sandbox").querySelector("td.sandbox span");
    expect(cell?.textContent).toBe("checking...");
    expect(cell?.className).toBe("faint");
  });

  test("renders cold failed sandbox status as unknown with error tooltip and warning", async () => {
    renderList({
      workspaces: [Workspace.EMPTY],
      listSessions: () => ({
        warnings: [],
        sandboxSnapshot: create(SandboxSnapshotSchema, {
          refreshing: false,
          stale: true,
          error: "sandbox status temporarily unavailable",
        }),
        sessions: [
          create(SessionInfoSchema, {
            id: "sb-unknown",
            name: "unknown-sandbox",
            workspace: Workspace.EMPTY,
            sandboxStatus: SandboxStatus.UNKNOWN,
          }),
        ],
      }),
    });

    await waitFor(() => expect(screen.getByRole("link", { name: "unknown-sandbox" })).toBeTruthy());

    const cell = sessionRow("unknown-sandbox").querySelector("td.sandbox span");
    expect(cell?.textContent).toBe("unknown");
    expect(cell?.className).toBe("faint");
    expect(cell?.getAttribute("title")).toBe("sandbox status temporarily unavailable");
    expect(screen.getByText("sandbox status temporarily unavailable").className).toBe("banner tone-warn");
  });

  test("renders all observed sandbox statuses and observedAt header tooltip", async () => {
    const observedMs = Date.now() - 10_000;
    const observedTs = timestampFromMs(observedMs);
    const { container } = renderList({
      workspaces: [Workspace.EMPTY, Workspace.REPO],
      listSessions: () => ({
        warnings: [],
        sandboxSnapshot: create(SandboxSnapshotSchema, {
          observedAt: observedTs,
          refreshing: false,
          stale: false,
        }),
        sessions: [
          create(SessionInfoSchema, { id: "s1", name: "sb-running", workspace: Workspace.EMPTY, sandboxStatus: SandboxStatus.RUNNING }),
          create(SessionInfoSchema, { id: "s2", name: "sb-starting", workspace: Workspace.EMPTY, sandboxStatus: SandboxStatus.STARTING }),
          create(SessionInfoSchema, { id: "s3", name: "sb-stopping", workspace: Workspace.EMPTY, sandboxStatus: SandboxStatus.STOPPING }),
          create(SessionInfoSchema, { id: "s4", name: "sb-stopped", workspace: Workspace.EMPTY, sandboxStatus: SandboxStatus.STOPPED }),
          create(SessionInfoSchema, { id: "s5", name: "sb-error", workspace: Workspace.EMPTY, sandboxStatus: SandboxStatus.ERROR }),
          create(SessionInfoSchema, { id: "s6", name: "sb-missing", workspace: Workspace.EMPTY, sandboxStatus: SandboxStatus.MISSING }),
          create(SessionInfoSchema, { id: "s7", name: "sb-none", workspace: Workspace.DIRECTORY, cwd: "/dir", sandboxStatus: SandboxStatus.UNSPECIFIED }),
        ],
      }),
    });

    await waitFor(() => expect(screen.getByRole("link", { name: "sb-running" })).toBeTruthy());

    const sandboxHeader = container.querySelector("thead th.sandbox-col");
    expect(sandboxHeader?.textContent).toBe("Sandbox");
    expect(sandboxHeader?.getAttribute("title")).toContain(timestampDate(observedTs).toLocaleString());

    expect(within(sessionRow("sb-running")).getByText("running").className).toBe("badge tone-ok");
    expect(within(sessionRow("sb-starting")).getByText("starting").className).toBe("badge tone-warn");
    expect(within(sessionRow("sb-stopping")).getByText("stopping").className).toBe("badge tone-muted");
    expect(within(sessionRow("sb-stopped")).getByText("stopped").className).toBe("faint");
    expect(within(sessionRow("sb-error")).getByText("error").className).toBe("badge tone-error");

    const absent = within(sessionRow("sb-missing")).getByText("absent");
    expect(absent.className).toBe("faint");
    expect(absent.getAttribute("title")).toBe("Sandbox not found on gateway; a fresh one will be created when opened");

    expect(sessionRow("sb-none").querySelector("td.sandbox span")?.textContent).toBe("-");
    expect(sessionRow("sb-none").querySelector("td.sandbox span")?.className).toBe("faint");
  });

  test("preserves cached row states with stale styling and shows snapshot error alongside warnings", async () => {
    const observedMs = Date.now() - 120_000;
    const { container } = renderList({
      workspaces: [Workspace.EMPTY],
      listSessions: () => ({
        warnings: ["skipped bad/session.json"],
        sandboxSnapshot: create(SandboxSnapshotSchema, {
          observedAt: timestampFromMs(observedMs),
          refreshing: false,
          stale: true,
          error: "sandbox status temporarily unavailable",
        }),
        sessions: [
          create(SessionInfoSchema, { id: "s1", name: "cached-run", workspace: Workspace.EMPTY, sandboxStatus: SandboxStatus.RUNNING }),
          create(SessionInfoSchema, { id: "s2", name: "cached-dir", workspace: Workspace.DIRECTORY, cwd: "/dir", sandboxStatus: SandboxStatus.UNSPECIFIED }),
        ],
      }),
    });

    await waitFor(() => expect(screen.getByRole("link", { name: "cached-run" })).toBeTruthy());

    const sandboxHeader = container.querySelector("thead th.sandbox-col");
    expect(sandboxHeader?.textContent).toBe("Sandboxcached 2m ago");

    expect(within(sessionRow("cached-run")).getByText("running").className).toBe("badge tone-ok stale");
    expect(sessionRow("cached-dir").querySelector("td.sandbox span")?.className).toBe("faint");
    expect(screen.getByText("skipped bad/session.json").className).toBe("banner tone-warn");
    expect(screen.getByText("sandbox status temporarily unavailable").className).toBe("banner tone-warn");
  });

  test("Refresh button sends refreshSandboxStatus: true without changing regular poll requests", async () => {
    const user = userEvent.setup();
    const calls: boolean[] = [];
    let refreshed = false;
    const { qc } = renderList({
      workspaces: [Workspace.EMPTY],
      listSessions: (req) => {
        calls.push(req.refreshSandboxStatus);
        if (req.refreshSandboxStatus) refreshed = true;
        if (refreshed) {
          return {
            warnings: [],
            sandboxSnapshot: create(SandboxSnapshotSchema, {
              observedAt: timestampFromMs(Date.now() - 30_000),
              refreshing: true,
              stale: true,
            }),
            sessions: [
              create(SessionInfoSchema, {
                id: "s1",
                name: "sb-one",
                workspace: Workspace.EMPTY,
                sandboxStatus: SandboxStatus.RUNNING,
              }),
            ],
          };
        }
        return {
          warnings: [],
          sandboxSnapshot: create(SandboxSnapshotSchema, {
            observedAt: timestampFromMs(Date.now() - 30_000),
            refreshing: false,
            stale: false,
          }),
          sessions: [
            create(SessionInfoSchema, {
              id: "s1",
              name: "sb-one",
              workspace: Workspace.EMPTY,
              sandboxStatus: SandboxStatus.STOPPED,
            }),
          ],
        };
      },
    });

    await waitFor(() => expect(within(sessionRow("sb-one")).getByText("stopped")).toBeTruthy());
    expect(calls).toEqual([false]);

    await user.click(screen.getByRole("button", { name: "Refresh" }));

    await waitFor(() => expect(within(sessionRow("sb-one")).getByText("running")).toBeTruthy());
    expect(screen.getByText("updating...")).toBeTruthy();
    expect(calls).toEqual([false, true, false]);

    // Triggering a regular query refetch still uses refreshSandboxStatus: false.
    refreshed = false;
    await qc.refetchQueries();
    await waitFor(() => expect(within(sessionRow("sb-one")).getByText("stopped")).toBeTruthy());
    expect(calls).toEqual([false, true, false, false]);
  });

  test("Refresh button surfaces RPC failures cleanly without leaving the button disabled", async () => {
    const user = userEvent.setup();
    renderList({
      workspaces: [Workspace.EMPTY],
      listSessions: (req) => {
        if (req.refreshSandboxStatus) {
          throw new ConnectError("server busy", Code.Unavailable);
        }
        return {
          warnings: [],
          sessions: [
            create(SessionInfoSchema, {
              id: "s1",
              name: "sb-one",
              workspace: Workspace.EMPTY,
              sandboxStatus: SandboxStatus.RUNNING,
            }),
          ],
        };
      },
    });

    await waitFor(() => expect(within(sessionRow("sb-one")).getByText("running")).toBeTruthy());

    const btn = screen.getByRole("button", { name: "Refresh" }) as HTMLButtonElement;
    await user.click(btn);

    await waitFor(() => expect(screen.getByText(/server busy/).className).toBe("banner tone-error"));
    expect(btn.disabled).toBe(false);
  });
});
