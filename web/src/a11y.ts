import type { KeyboardEvent } from "react";

export const activate = (fn: (e: KeyboardEvent) => void) => (e: KeyboardEvent) => {
  if (e.key === "Enter" || e.key === " ") {
    e.preventDefault();
    fn(e);
  }
};
