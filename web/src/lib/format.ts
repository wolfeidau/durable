export function compact(n: number): string {
  const units: [number, string][] = [
    [1e9, "B"],
    [1e6, "M"],
    [1e3, "K"],
  ];
  for (const [size, suffix] of units) {
    if (n >= size) {
      const v = n / size;
      return `${v >= 100 || Number.isInteger(v) ? Math.round(v) : v.toFixed(1)}${suffix}`;
    }
  }
  return String(Math.round(n));
}

export const dash = "—";

export function money(usd: number | undefined, estimated = false): string {
  if (usd === undefined) return dash;
  return `${estimated ? "~" : ""}$${usd.toFixed(usd < 1 ? 4 : 2)}`;
}

// "33ms" under a second, "2.1s" under ten seconds, "42s" under a minute, then "1m 05s".
export function duration(ms: number): string {
  const rounded = Math.max(0, Math.round(ms));
  if (rounded < 1000) return `${rounded}ms`;
  const secs = rounded / 1000;
  if (secs < 10) return `${secs.toFixed(1)}s`;
  const whole = Math.round(secs);
  return whole < 60 ? `${whole}s` : `${Math.floor(whole / 60)}m ${String(whole % 60).padStart(2, "0")}s`;
}

export const seconds = (ms: number | undefined): string => (ms === undefined ? dash : duration(ms));

// A small non-zero share reads "<1%", because "0%" looks like nothing is used.
export const percent = (f: number | undefined): string =>
  f === undefined ? dash : f > 0 && f < 0.005 ? "<1%" : `${Math.round(f * 100)}%`;

export const plural = (n: number | bigint, word: string): string => `${n} ${Number(n) === 1 ? word : `${word}s`}`;
