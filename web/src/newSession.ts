import { Workspace } from "./gen/durable/v1/durable_pb";

export interface NewSessionForm {
  cwd: string;
  name: string;
}

export interface NewSessionRequest {
  cwd: string;
  name: string;
  workspace: Workspace;
  requestId: string;
}

// Empty mode needs a sandboxed server, which offers no directory sessions.
export function emptyOnly(offered: readonly Workspace[]): boolean {
  return offered.includes(Workspace.EMPTY) && !offered.includes(Workspace.DIRECTORY);
}

export function canCreate(form: NewSessionForm, empty: boolean): boolean {
  return empty || form.cwd.trim() !== "";
}

// The caller keeps requestId across failed attempts so a retry is the same create.
export function buildRequest(form: NewSessionForm, empty: boolean, requestId: string): NewSessionRequest {
  const name = form.name.trim();
  if (empty) return { cwd: "", name, workspace: Workspace.EMPTY, requestId };
  return { cwd: form.cwd.trim(), name, workspace: Workspace.DIRECTORY, requestId: "" };
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
