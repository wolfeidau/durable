import { describe, expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type ReactElement } from "react";
import { VerdictFilter } from "./VerdictFilter";
import type { Verdict } from "./networkView";

function Harness({ seen }: { seen: Verdict[] }): ReactElement {
  const [v, setV] = useState<Verdict>("all");
  return (
    <VerdictFilter
      verdict={v}
      onVerdict={(next) => {
        seen.push(next);
        setV(next);
      }}
    />
  );
}

const checked = (name: string) => screen.getByRole("radio", { name }).getAttribute("aria-checked");

describe("VerdictFilter", () => {
  test("is a labelled group with the current verdict selected", () => {
    render(<Harness seen={[]} />);
    expect(screen.getByRole("radiogroup", { name: "Verdict" })).toBeTruthy();
    expect(checked("All")).toBe("true");
    expect(checked("Denied")).toBe("false");
  });

  test("arrow keys move focus and Space and Enter select", async () => {
    const user = userEvent.setup();
    const seen: Verdict[] = [];
    render(<Harness seen={seen} />);
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole("radio", { name: "All" }));
    await user.keyboard("{ArrowRight}");
    expect(document.activeElement).toBe(screen.getByRole("radio", { name: "Denied" }));
    expect(seen).toEqual([]);
    await user.keyboard(" ");
    expect(seen).toEqual(["denied"]);
    await user.keyboard("{ArrowRight}{Enter}");
    expect(seen).toEqual(["denied", "allowed"]);
    expect(checked("Allowed")).toBe("true");
  });

  test("clicking the selected verdict keeps it selected", async () => {
    const user = userEvent.setup();
    const seen: Verdict[] = [];
    render(<Harness seen={seen} />);
    await user.click(screen.getByRole("radio", { name: "All" }));
    expect(seen.filter((v) => v !== "all")).toEqual([]);
    expect(checked("All")).toBe("true");
    expect(checked("Denied")).toBe("false");
  });
});
