import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Workspace, type SessionInfo } from "./gen/durable/v1/durable_pb";

export function age(info: SessionInfo, now: number = Date.now()): string {
  if (!info.createdAt) return "";
  const secs = Math.max(0, Math.round((now - timestampDate(info.createdAt).getTime()) / 1000));
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

export const created = (s: SessionInfo) => (s.createdAt ? timestampDate(s.createdAt).getTime() : 0);

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
