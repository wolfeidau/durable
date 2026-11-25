import { describe, expect, test } from "bun:test";
import { createRouterTransport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { SessionService, Workspace } from "../gen/durable/v1/durable_pb";
import { App } from "./App";

const sessionId = "0123456789abcdef";

function makeTransport() {
  return createRouterTransport(({ service }) => {
    service(SessionService, {
      listSessions: () => ({
        sessions: [{ id: sessionId, name: "Audit retry backoff", cwd: "/work/app", workspace: Workspace.DIRECTORY }],
        warnings: [],
      }),
      getServerInfo: () => ({ settings: [], warnings: [], workspaces: [Workspace.DIRECTORY], environments: [], grants: [] }),
      async *watchEvents(_req, ctx) {
        yield { item: { case: "heartbeat", value: {} } };
        await new Promise<void>((resolve) => ctx.signal.addEventListener("abort", () => resolve(), { once: true }));
      },
    });
  });
}

describe("App session layout", () => {
  test("renders SessionBar inside main and outside the topbar", async () => {
    const prev = location.href;
    history.replaceState(null, "", `/?session=${sessionId}`);
    const origRO = globalThis.ResizeObserver;
    const origScrollTo = window.scrollTo;
    globalThis.ResizeObserver = class {
      observe() {}
      unobserve() {}
      disconnect() {}
    };
    window.scrollTo = () => {};
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    try {
      const { container } = render(
        <TransportProvider transport={makeTransport()}>
          <QueryClientProvider client={qc}>
            <App />
          </QueryClientProvider>
        </TransportProvider>,
      );

      await waitFor(() => expect(screen.getByText("/work/app")).toBeTruthy());

      const topbar = container.querySelector("header.topbar");
      const main = container.querySelector("main.page-session");
      const sessionBar = container.querySelector(".session-bar");
      expect(topbar).not.toBeNull();
      expect(main).not.toBeNull();
      expect(sessionBar).not.toBeNull();
      expect(topbar!.contains(sessionBar)).toBe(false);
      expect(main!.contains(sessionBar)).toBe(true);
      expect(topbar!.querySelector("a.crumb-mid")?.textContent).toBe("Sessions");
      expect(topbar!.querySelector(".crumb-name")?.textContent).toBe("Audit retry backoff");
    } finally {
      globalThis.ResizeObserver = origRO;
      window.scrollTo = origScrollTo;
      history.replaceState(null, "", prev);
    }
  });
});
