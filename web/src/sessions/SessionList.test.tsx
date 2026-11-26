import { describe, expect, test } from "bun:test";
import { createRouterTransport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { SessionService, Workspace } from "../gen/durable/v1/durable_pb";
import { SessionList } from "./SessionList";

function renderList(workspaces: Workspace[]) {
  const transport = createRouterTransport(({ service }) => {
    service(SessionService, {
      listSessions: () => ({ sessions: [], warnings: [] }),
      getServerInfo: () => ({ settings: [], warnings: [], workspaces, environments: [], grants: [] }),
    });
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <TransportProvider transport={transport}>
      <QueryClientProvider client={qc}>
        <SessionList onSelect={() => {}} />
      </QueryClientProvider>
    </TransportProvider>,
  );
}

describe("NewSessionForm workspace caption", () => {
  test("omits the static caption when directory is the only workspace", async () => {
    const { container } = renderList([Workspace.DIRECTORY]);
    await waitFor(() => expect(screen.getByLabelText("Working directory")).toBeTruthy());
    expect(container.querySelector("#new-session .kind")).toBeNull();
  });

  test("shows Repository when repo is the only workspace", async () => {
    const { container } = renderList([Workspace.REPO]);
    await waitFor(() => expect(screen.getByLabelText("Repository")).toBeTruthy());
    expect(container.querySelector("#new-session .kind")?.textContent).toBe("Repository");
  });

  test("shows Empty workspace when empty is the only workspace", async () => {
    const { container } = renderList([Workspace.EMPTY]);
    await waitFor(() => expect(screen.getByRole("button", { name: "Create" })).toBeTruthy());
    expect(container.querySelector("#new-session .kind")?.textContent).toBe("Empty workspace");
  });
});
