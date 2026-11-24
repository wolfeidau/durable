import { describe, expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type ReactElement } from "react";
import { ViewPanel, ViewTabs } from "./ViewTabs";

function Harness({ sandboxed, calls, initial }: { sandboxed: boolean; calls: unknown[]; initial?: "report" | "network" }): ReactElement {
  const [tab, setTab] = useState(initial);
  return (
    <ViewTabs
      view={tab ?? "transcript"}
      onTab={(t) => {
        calls.push(t);
        setTab(t);
      }}
      sandboxed={sandboxed}
      deniedCount={2}
    >
      <ViewPanel id="report">
        <p>report body</p>
      </ViewPanel>
      <ViewPanel id="transcript">
        <p>transcript body</p>
      </ViewPanel>
    </ViewTabs>
  );
}

describe("ViewTabs", () => {
  test("arrow keys move focus between tabs, and Enter or Space opens one", async () => {
    const user = userEvent.setup();
    const calls: unknown[] = [];
    render(<Harness sandboxed calls={calls} />);
    await user.tab();
    expect(screen.getByRole("tab", { name: "Transcript" })).toHaveProperty("tabIndex", 0);
    await user.keyboard("{ArrowRight}{ArrowRight}");
    expect(document.activeElement).toBe(screen.getByRole("tab", { name: /Network/ }));
    expect(calls).toEqual([]);
    expect(screen.getByRole("tab", { name: "Transcript" }).getAttribute("aria-selected")).toBe("true");
    await user.keyboard("{Enter}");
    expect(calls).toEqual(["network"]);
    await user.keyboard("{ArrowLeft}");
    expect(calls).toEqual(["network"]);
    await user.keyboard(" ");
    expect(calls).toEqual(["network", "report"]);
    expect(screen.getByRole("tab", { name: "Report" }).getAttribute("aria-selected")).toBe("true");
    await user.keyboard("{ArrowLeft}{Enter}");
    expect(calls).toEqual(["network", "report", undefined]);
  });

  test("the Network tab is absent unless sandboxed, and shows the denied count", () => {
    const calls: unknown[] = [];
    const { unmount } = render(<Harness sandboxed={false} calls={calls} />);
    expect(screen.queryByRole("tab", { name: /Network/ })).toBeNull();
    unmount();
    render(<Harness sandboxed calls={calls} />);
    expect(screen.getByRole("tab", { name: "Network · 2 denied" })).toBeTruthy();
  });

  test("a requested Network view keeps its tab without a sandbox", () => {
    render(<Harness sandboxed={false} calls={[]} initial="network" />);
    expect(screen.getByRole("tab", { name: /Network/ }).getAttribute("aria-selected")).toBe("true");
  });

  test("the transcript stays mounted while Report is selected", async () => {
    const user = userEvent.setup();
    render(<Harness sandboxed={false} calls={[]} />);
    expect(screen.queryByText("report body")).toBeNull();
    await user.click(screen.getByRole("tab", { name: "Report" }));
    expect(screen.getByText("report body")).toBeTruthy();
    expect(screen.getByText("transcript body").closest("[inert]")).not.toBeNull();
    expect(screen.getByText("report body").closest("[inert]")).toBeNull();
  });
});
