import { describe, expect, test } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactElement } from "react";
import { SubmissionStatus } from "../../gen/durable/v1/durable_pb";
import { TipHost } from "../../ui/Tooltip";
import { CallLists } from "./CallLists";
import type { CallRow, Overview, TurnBar } from "./overview";
import { TipRow } from "./parts";
import { Timeline } from "./Timeline";
import { ToolChart } from "./ToolChart";
import type { Report } from "./usage";

const call: CallRow = { id: "c1", seq: 3n, n: 1, name: "bash", preview: "ls -la", ms: 1200, isError: true, error: "exit 1: nope" };
const turn: TurnBar = {
  seq: 2n,
  n: 1,
  prompt: "list files",
  status: SubmissionStatus.UNSPECIFIED,
  errors: 0,
  calls: 1,
  estimated: false,
  segModel: 1,
  segTool: 1,
  segOther: 0,
  totalMs: 2000,
};
const overview: Overview = {
  turns: [turn],
  calls: [call],
  slowest: [call],
  failed: [call],
  context: [],
  compactions: [],
  errors: 1,
  totalCalls: 1,
};
const report: Report = {
  tools: [
    { name: "bash", calls: 1, errors: 1, total: 1200 },
    { name: "orphan", calls: 1, errors: 0, total: 5 },
  ],
  model: { replies: 0 },
  turns: [],
};

const wrap = (el: ReactElement) => <TipHost>{el}</TipHost>;

async function activates(button: HTMLElement, jumps: string[], target: string) {
  const user = userEvent.setup();
  act(() => button.focus());
  await user.keyboard("{Enter}");
  expect(jumps).toEqual([target]);
  await user.keyboard(" ");
  expect(jumps).toEqual([target, target]);
}

describe("report rows", () => {
  test("a timeline row is a named button that opens its turn", async () => {
    const jumps: string[] = [];
    render(wrap(<Timeline overview={overview} onJump={(t) => jumps.push(t)} />));
    await activates(screen.getByRole("button", { name: "Turn 1: list files. Open in transcript" }), jumps, "turn-2");
  });

  test("call list rows are named buttons that open their call", async () => {
    const jumps: string[] = [];
    render(wrap(<CallLists overview={overview} onJump={(t) => jumps.push(t)} />));
    const rows = screen.getAllByRole("button", { name: /bash.*ls -la/ });
    expect(rows).toHaveLength(2);
    await activates(rows[0]!, jumps, "call-c1");
  });

  test("a tool row is a button only when it has a call to open", async () => {
    const jumps: string[] = [];
    render(wrap(<ToolChart report={report} overview={overview} onJump={(t) => jumps.push(t)} />));
    expect(screen.getAllByRole("button")).toHaveLength(1);
    await activates(screen.getByRole("button", { name: /^bash/ }), jumps, "call-c1");
  });

  test("TipRow renders label before value", () => {
    const { container } = render(<TipRow label="errors" value={1} />);
    expect(container.querySelector(".tip-row")?.textContent).toBe("errors1");
    expect(container.querySelector(".tip-label")?.textContent).toBe("errors");
    expect(container.querySelector("strong")?.textContent).toBe("1");
  });
});
