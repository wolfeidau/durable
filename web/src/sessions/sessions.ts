import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import {
  SandboxStatus,
  SubmissionStatus,
  Workspace,
  type SandboxSnapshot,
  type SessionInfo,
} from "../gen/durable/v1/durable_pb";

export const fastPollMs = 2_000;
export const normalPollMs = 15_000;

export function pollInterval(snapshot: SandboxSnapshot | undefined): number {
  return snapshot?.refreshing ? fastPollMs : normalPollMs;
}

function relativeAge(ts: Timestamp | undefined, now: number = Date.now()): string {
  if (!ts) return "";
  const secs = Math.max(0, Math.round((now - timestampDate(ts).getTime()) / 1000));
  if (secs < 60) return "just now";
  const units: [string, number][] = [
    ["d", 86400],
    ["h", 3600],
    ["m", 60],
  ];
  for (const [unit, size] of units) {
    if (secs >= size) return `${Math.floor(secs / size)}${unit} ago`;
  }
  return "";
}

export function age(info: SessionInfo, now: number = Date.now()): string {
  return relativeAge(info.createdAt, now);
}

export interface StatusView {
  text: string;
  className: string;
  title?: string;
}

export function workStatus(s: SessionInfo): StatusView {
  if (s.pendingApprovals > 0) return { text: `${s.pendingApprovals} awaiting approval`, className: "badge tone-warn" };
  if (s.unfinishedSubmissions > 0) return { text: "working", className: "badge tone-link pulse" };
  switch (s.lastSubmissionStatus) {
    case SubmissionStatus.FAILED:
      return { text: "failed", className: "badge tone-error" };
    case SubmissionStatus.CANCELLED:
      return { text: "cancelled", className: "badge tone-cancelled" };
    case SubmissionStatus.DONE:
      return { text: "done", className: "muted" };
    default:
      return { text: "new", className: "faint" };
  }
}

export function showSandboxColumn(sessions: readonly SessionInfo[], snapshot: SandboxSnapshot | undefined): boolean {
  return snapshot != null || sessions.some((s) => s.sandboxStatus !== SandboxStatus.UNSPECIFIED);
}

export function sandboxHeaderNote(snapshot: SandboxSnapshot | undefined, now: number = Date.now()): string | undefined {
  if (!snapshot) return undefined;
  if (!snapshot.observedAt) return snapshot.refreshing ? "checking..." : undefined;
  if (!snapshot.stale) return undefined;
  return snapshot.refreshing ? "updating..." : `cached ${relativeAge(snapshot.observedAt, now)}`;
}

const missingSandboxTitle = "Sandbox not found on gateway; a fresh one will be created when opened";

export function sandboxStatusView(status: SandboxStatus, snapshot: SandboxSnapshot | undefined): StatusView {
  const stale = snapshot?.stale ? " stale" : "";
  switch (status) {
    case SandboxStatus.UNKNOWN:
      return {
        text: snapshot?.refreshing ? "checking..." : "unknown",
        className: "faint",
        title: snapshot?.error || undefined,
      };
    case SandboxStatus.RUNNING:
      return { text: "running", className: `badge tone-ok${stale}` };
    case SandboxStatus.STARTING:
      return { text: "starting", className: `badge tone-warn${stale}` };
    case SandboxStatus.STOPPING:
      return { text: "stopping", className: `badge tone-muted${stale}` };
    case SandboxStatus.STOPPED:
      return { text: "stopped", className: `faint${stale}` };
    case SandboxStatus.ERROR:
      return { text: "error", className: `badge tone-error${stale}` };
    case SandboxStatus.MISSING:
      return { text: "absent", className: `faint${stale}`, title: missingSandboxTitle };
    default:
      return { text: "-", className: "faint" };
  }
}

export function sessionWarnings(warnings: readonly string[] | undefined, snapshot: SandboxSnapshot | undefined): string[] {
  const out = [...(warnings ?? [])];
  if (snapshot?.error && !out.includes(snapshot.error)) out.push(snapshot.error);
  return out;
}

const created = (s: SessionInfo) => (s.createdAt ? timestampDate(s.createdAt).getTime() : 0);

// Sessions waiting on a person come first, then the newest.
export function sorted(sessions: SessionInfo[]): SessionInfo[] {
  return [...sessions].sort(
    (a, b) => Number(b.pendingApprovals > 0) - Number(a.pendingApprovals > 0) || created(b) - created(a),
  );
}

export function where(s: SessionInfo): string {
  switch (s.workspace) {
    case Workspace.EMPTY:
      return "empty workspace";
    case Workspace.REPO:
      return `${s.repo}@${s.branch}`;
    default:
      return s.cwd;
  }
}

// Each path segment is encoded on its own, because a branch may hold "/" and "#".
export const repoHref = (s: SessionInfo): string =>
  `https://${s.repo}/tree/${s.branch.split("/").map(encodeURIComponent).join("/")}`;

export function kindLabel(s: SessionInfo): "Directory" | "Empty" | "Repo" {
  switch (s.workspace) {
    case Workspace.EMPTY:
      return "Empty";
    case Workspace.REPO:
      return "Repo";
    default:
      return "Directory";
  }
}

export function kindText(s: SessionInfo): string {
  return s.environment ? `${kindLabel(s)} · ${s.environment}` : kindLabel(s);
}
