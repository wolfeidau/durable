import type { ReactElement } from "react";
import { ToggleButton, ToggleButtonGroup } from "react-aria-components";
import type { Verdict } from "./networkView";

const verdicts: { value: Verdict; label: string }[] = [
  { value: "all", label: "All" },
  { value: "denied", label: "Denied" },
  { value: "allowed", label: "Allowed" },
];

export function VerdictFilter({ verdict, onVerdict }: { verdict: Verdict; onVerdict: (v: Verdict) => void }): ReactElement {
  return (
    <ToggleButtonGroup
      className="tabs net-verdicts"
      aria-label="Verdict"
      selectionMode="single"
      disallowEmptySelection
      selectedKeys={[verdict]}
      onSelectionChange={(keys) => {
        const [next] = keys;
        if (next !== undefined) onVerdict(next as Verdict);
      }}
    >
      {verdicts.map((v) => (
        <ToggleButton key={v.value} id={v.value} className="secondary">
          {v.label}
        </ToggleButton>
      ))}
    </ToggleButtonGroup>
  );
}
