import { ApprovalStatus } from "../../gen/durable/v1/durable_pb";
import type { CallEntry } from "./transcript";
import { duration } from "../../lib/format";

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
