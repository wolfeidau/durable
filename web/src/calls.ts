import { ApprovalStatus } from "./gen/durable/v1/durable_pb";
import type { CallEntry } from "./transcript";

// "33ms" under a second, "2.1s" under ten seconds, "42s" under a minute, then "1m 05s".
export function duration(ms: number): string {
  const rounded = Math.max(0, Math.round(ms));
  if (rounded < 1000) return `${rounded}ms`;
  const secs = rounded / 1000;
  if (secs < 10) return `${secs.toFixed(1)}s`;
  const whole = Math.round(secs);
  return whole < 60 ? `${whole}s` : `${Math.floor(whole / 60)}m ${String(whole % 60).padStart(2, "0")}s`;
}

export function isRunning(call: CallEntry): boolean {
  const s = call.approval?.status;
  return call.result === undefined && s !== ApprovalStatus.PENDING && s !== ApprovalStatus.DECLINED && s !== ApprovalStatus.CANCELLED;
}

const timed = (label: string, ms: number | undefined) => (ms === undefined ? label : `${label} ${duration(ms)}`);

export function chip(call: CallEntry, output: string | undefined, now: number = Date.now()): { text: string; tone: string } {
  const a = call.approval;
  if (a?.status === ApprovalStatus.PENDING) return { text: "waiting", tone: "tone-warn" };
  if (a?.status === ApprovalStatus.DECLINED) return { text: "denied", tone: "tone-error" };
  if (a?.status === ApprovalStatus.CANCELLED) return { text: "cancelled", tone: "tone-cancelled" };
  if (call.result !== undefined) {
    const ms = call.startedAt !== undefined && call.doneAt !== undefined ? call.doneAt - call.startedAt : undefined;
    return call.isError ? { text: timed("error", ms), tone: "tone-error" } : { text: timed("done", ms), tone: "tone-ok" };
  }
  const ms = call.startedAt === undefined ? undefined : now - call.startedAt;
  return { text: timed("running", ms), tone: output ? "tone-link pulse" : "tone-link" };
}

const previewKeys = ["command", "path", "pattern", "file_path", "url", "query"];
const previewMax = 80;

export function summary(call: CallEntry): string {
  if (call.rewritten || call.input === undefined) return "";
  let obj: unknown;
  try {
    obj = JSON.parse(call.input);
  } catch {
    return "";
  }
  if (typeof obj !== "object" || obj === null || Array.isArray(obj)) return "";
  const fields = obj as Record<string, unknown>;
  const key = previewKeys.find((k) => typeof fields[k] === "string");
  const value = key ? fields[key] : Object.values(fields).find((v) => typeof v === "string");
  if (typeof value !== "string") return "";
  const text = value.replace(/\s+/g, " ").trim();
  return text.length > previewMax ? `${text.slice(0, previewMax)}…` : text;
}

export function defaultOpen(call: CallEntry, output: string | undefined): boolean {
  const s = call.approval?.status;
  if (s === ApprovalStatus.DECLINED || s === ApprovalStatus.CANCELLED) return false;
  return call.result === undefined || !!output || call.isError;
}
