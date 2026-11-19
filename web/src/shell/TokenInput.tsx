import { useState, type ReactElement } from "react";

export function TokenInput({ onApply }: { onApply: (t: string) => void }): ReactElement {
  const [value, setValue] = useState("");
  return (
    <form
      className="banner tone-warn token-box"
      onSubmit={(e) => {
        e.preventDefault();
        onApply(value.trim());
      }}
    >
      <label>
        Token needed
        <input type="password" autoComplete="off" value={value} onChange={(e) => setValue(e.target.value)} />
      </label>
      <button type="submit">Apply</button>
    </form>
  );
}
