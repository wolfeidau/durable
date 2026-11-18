import { type EnvironmentInfo } from "../gen/durable/v1/durable_pb";

// An empty approvals value means ask.
export const approvalsLabel = (e: EnvironmentInfo): string => (e.approvals === "auto" ? "auto" : "ask");

// Auto-approve is visible in the picker because it changes what a session may do unasked.
export const environmentLabel = (e: EnvironmentInfo): string => (e.approvals === "auto" ? `${e.name} · auto-approve` : e.name);

export function environmentHint(e: EnvironmentInfo): string {
  return `${e.image} · ${e.grants.length > 0 ? e.grants.join(", ") : "no network grants"}`;
}
