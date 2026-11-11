import { Workspace } from "./gen/durable/v1/durable_pb";

export type Mode = "directory" | "empty" | "repo";

export interface NewSessionForm {
  mode: Mode;
  cwd: string;
  name: string;
  repo: string;
  branch: string;
}

export interface NewSessionRequest {
  cwd: string;
  name: string;
  workspace: Workspace;
  requestId: string;
  repo: string;
  branch: string;
}

export const defaultBranch = "main";

const kinds: [Workspace, Mode][] = [
  [Workspace.DIRECTORY, "directory"],
  [Workspace.EMPTY, "empty"],
  [Workspace.REPO, "repo"],
];

// The modes the server offers, in the order the form lists them.
export function modesFor(offered: readonly Workspace[]): Mode[] {
  return kinds.filter(([w]) => offered.includes(w)).map(([, m]) => m);
}

export function canCreate(form: NewSessionForm): boolean {
  switch (form.mode) {
    case "empty":
      return true;
    case "repo":
      return form.repo.trim() !== "" && form.branch.trim() !== "";
    default:
      return form.cwd.trim() !== "";
  }
}

// The caller keeps requestId across failed attempts so a retry is the same create. Only an empty session uses it.
export function buildRequest(form: NewSessionForm, requestId: string): NewSessionRequest {
  const name = form.name.trim();
  const none = { cwd: "", requestId: "", repo: "", branch: "" };
  switch (form.mode) {
    case "empty":
      return { ...none, name, workspace: Workspace.EMPTY, requestId };
    case "repo":
      return { ...none, name, workspace: Workspace.REPO, repo: form.repo.trim(), branch: form.branch.trim() };
    default:
      return { ...none, name, workspace: Workspace.DIRECTORY, cwd: form.cwd.trim() };
  }
}

// crypto.randomUUID exists only in secure contexts, and the server may be reached over plain HTTP on a LAN address.
function randomId(): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, "0")).join("");
}

// Reuses the ID until a create succeeds, so a failed attempt retries as the same create.
export class RequestIds {
  private current: string | undefined;

  constructor(private readonly generate: () => string = randomId) {}

  get(): string {
    this.current ??= this.generate();
    return this.current;
  }

  succeeded(): void {
    this.current = undefined;
  }
}
