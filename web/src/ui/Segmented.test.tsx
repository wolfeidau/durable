import { describe, expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type ReactElement } from "react";
import { Segmented } from "./Segmented";

const options = [
  { value: "a", label: "Alpha", title: "first" },
  { value: "b", label: "Beta" },
  { value: "c", label: "Gamma" },
];

function Harness({ seen }: { seen: string[] }): ReactElement {
  const [v, setV] = useState("a");
  return (
    <>
      <span id="caption">Pick</span>
      <Segmented
        aria-labelledby="caption"
        options={options}
        value={v}
        onChange={(next) => {
          seen.push(next);
          setV(next);
        }}
      />
    </>
  );
}

describe("Segmented", () => {
  test("is named by its caption and exposes the option title", () => {
    render(<Harness seen={[]} />);
    expect(screen.getByRole("radiogroup", { name: "Pick" })).toBeTruthy();
    expect(screen.getByText("Alpha").getAttribute("title")).toBe("first");
    expect(screen.getByRole("radio", { name: "Alpha" })).toHaveProperty("checked", true);
  });

  test("arrow keys change the value", async () => {
    const user = userEvent.setup();
    const seen: string[] = [];
    render(<Harness seen={seen} />);
    await user.tab();
    await user.keyboard("{ArrowRight}");
    expect(seen).toEqual(["b"]);
    expect(screen.getByRole("radio", { name: "Beta" })).toHaveProperty("checked", true);
    await user.keyboard("{ArrowLeft}");
    await user.keyboard("{ArrowLeft}");
    expect(seen).toEqual(["b", "a", "c"]);
  });
});
