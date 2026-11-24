import type { ReactElement } from "react";
import { Radio, RadioGroup } from "react-aria-components";

export interface Option<T extends string> {
  value: T;
  label: string;
  title?: string;
}

interface Props<T extends string> {
  options: Option<T>[];
  value: T;
  onChange: (value: T) => void;
  "aria-label"?: string;
  "aria-labelledby"?: string;
  className?: string;
}

export function Segmented<T extends string>({ options, value, onChange, className = "segmented", ...name }: Props<T>): ReactElement {
  return (
    <RadioGroup className={className} orientation="horizontal" value={value} onChange={(v) => onChange(v as T)} {...name}>
      {options.map((o) => (
        <Radio key={o.value} value={o.value}>
          <span title={o.title}>{o.label}</span>
        </Radio>
      ))}
    </RadioGroup>
  );
}
