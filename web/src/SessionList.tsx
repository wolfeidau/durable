import { useMutation, useQuery } from "@connectrpc/connect-query";
import { ConnectError } from "@connectrpc/connect";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useRef, useState, type FormEvent, type ReactElement } from "react";
import { createSession, getServerInfo, listSessions } from "./gen/durable/v1/durable-SessionService_connectquery";
import { Workspace, type SessionInfo } from "./gen/durable/v1/durable_pb";
import { buildRequest, canCreate, defaultBranch, modesFor, RequestIds, type Mode } from "./newSession";

const modeLabels: Record<Mode, string> = { directory: "Directory", empty: "Empty workspace", repo: "Repository" };

const cwdKey = "durable.lastCwd";

function lastCwd(): string {
  try {
    return localStorage.getItem(cwdKey) ?? "";
  } catch {
    return "";
  }
}

function rememberCwd(cwd: string): void {
  try {
    localStorage.setItem(cwdKey, cwd);
  } catch {
    // Storage can be blocked; the form then starts empty.
  }
}

function age(info: SessionInfo): string {
  if (!info.createdAt) return "";
  const secs = Math.max(0, Math.round((Date.now() - timestampDate(info.createdAt).getTime()) / 1000));
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

const created = (s: SessionInfo) => (s.createdAt ? timestampDate(s.createdAt).getTime() : 0);

// Sessions waiting on a person come first, then the newest.
function sorted(sessions: SessionInfo[]): SessionInfo[] {
  return [...sessions].sort(
    (a, b) => Number(b.pendingApprovals > 0) - Number(a.pendingApprovals > 0) || created(b) - created(a),
  );
}

function NewSessionForm({ onCreated }: { onCreated: (id: string) => void }): ReactElement {
  const [cwd, setCwd] = useState(lastCwd);
  const [name, setName] = useState("");
  const [repo, setRepo] = useState("");
  const [branch, setBranch] = useState(defaultBranch);
  const [chosen, setChosen] = useState<Mode | undefined>();
  const create = useMutation(createSession);
  const info = useQuery(getServerInfo, {}, { staleTime: Infinity });
  const requestIds = useRef(new RequestIds());
  const modes = modesFor(info.data?.workspaces ?? []);
  const mode = chosen && modes.includes(chosen) ? chosen : (modes[0] ?? "directory");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const form = { mode, cwd, name, repo, branch };
    const res = await create.mutateAsync(buildRequest(form, requestIds.current.get())).catch(() => undefined);
    if (!res?.session) return;
    requestIds.current.succeeded();
    if (mode === "directory") rememberCwd(cwd.trim());
    setName("");
    onCreated(res.session.id);
  };

  // Until the server says which kinds it offers, the form could show the wrong fields.
  if (info.isPending) return <></>;

  return (
    <form className="new-session" onSubmit={(e) => void submit(e)}>
      <h2>New session</h2>
      {modes.length > 1 && (
        <label>
          Workspace
          <select value={mode} onChange={(e) => setChosen(e.target.value as Mode)}>
            {modes.map((m) => (
              <option key={m} value={m}>
                {modeLabels[m]}
              </option>
            ))}
          </select>
        </label>
      )}
      {mode === "directory" && (
        <label>
          Working directory
          <input value={cwd} placeholder="/absolute/path" onChange={(e) => setCwd(e.target.value)} />
        </label>
      )}
      {mode === "empty" && modes.length === 1 && <p className="muted">Empty workspace</p>}
      {mode === "repo" && (
        <>
          <label>
            Repository
            <input value={repo} placeholder="github.com/owner/name" onChange={(e) => setRepo(e.target.value)} />
          </label>
          <label>
            Branch
            <input value={branch} onChange={(e) => setBranch(e.target.value)} />
          </label>
        </>
      )}
      <label>
        Name (optional)
        <input value={name} onChange={(e) => setName(e.target.value)} />
      </label>
      <button type="submit" disabled={!canCreate({ mode, cwd, name, repo, branch }) || create.isPending}>
        Create
      </button>
      {create.error && <p className="error">{ConnectError.from(create.error).rawMessage}</p>}
    </form>
  );
}

function where(s: SessionInfo): string {
  switch (s.workspace) {
    case Workspace.EMPTY:
      return "empty workspace";
    case Workspace.REPO:
      return `${s.repo}@${s.branch}`;
    default:
      return s.cwd;
  }
}

export function SessionList({ onSelect }: { onSelect: (id: string) => void }): ReactElement {
  const { data, error, isPending, refetch } = useQuery(listSessions, {}, { refetchInterval: 15_000 });

  return (
    <div className="session-list">
      <div className="row-head">
        <h2>Sessions</h2>
        <button onClick={() => void refetch()}>Refresh</button>
      </div>
      {isPending && !error && <p className="muted">Loading</p>}
      {error && <p className="error">{ConnectError.from(error).rawMessage}</p>}
      {data?.warnings.map((w) => (
        <p className="warn-text" key={w}>
          {w}
        </p>
      ))}
      {data && data.sessions.length === 0 && <p className="muted">No sessions yet.</p>}
      <ul>
        {sorted(data?.sessions ?? []).map((s) => (
          <li key={s.id}>
            <a
              href={`?session=${encodeURIComponent(s.id)}`}
              onClick={(e) => {
                e.preventDefault();
                onSelect(s.id);
              }}
            >
              <span className="session-name">{s.name || s.id}</span>
              <span className="session-cwd">{where(s)}</span>
              <span className="session-meta">
                {s.pendingApprovals > 0 && <span className="badge attention">{s.pendingApprovals} waiting for approval</span>}
                {s.unfinishedSubmissions > 0 && <span className="badge working">working</span>}
                <span className="muted">{age(s)}</span>
              </span>
            </a>
          </li>
        ))}
      </ul>
      <NewSessionForm
        onCreated={(id) => {
          void refetch();
          onSelect(id);
        }}
      />
    </div>
  );
}
