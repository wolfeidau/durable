import { useMutation, useQuery } from "@connectrpc/connect-query";
import { ConnectError } from "@connectrpc/connect";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useRef, useState, type FormEvent, type ReactElement } from "react";
import { createSession, getServerInfo, listSessions } from "./gen/durable/v1/durable-SessionService_connectquery";
import { type SessionInfo } from "./gen/durable/v1/durable_pb";
import {
  buildRequest,
  canCreate,
  defaultBranch,
  environmentHint,
  modesFor,
  pickEnvironment,
  RequestIds,
  type Mode,
} from "./newSession";
import { age, kindText, sorted, where } from "./sessions";

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

function NewSessionForm({ onCreated }: { onCreated: (id: string) => void }): ReactElement {
  const [cwd, setCwd] = useState(lastCwd);
  const [name, setName] = useState("");
  const [repo, setRepo] = useState("");
  const [branch, setBranch] = useState(defaultBranch);
  const [chosen, setChosen] = useState<Mode | undefined>();
  const [chosenEnv, setChosenEnv] = useState<string | undefined>();
  const create = useMutation(createSession);
  const info = useQuery(getServerInfo, {}, { staleTime: Infinity });
  const requestIds = useRef(new RequestIds());
  const modes = modesFor(info.data?.workspaces ?? []);
  const mode = chosen && modes.includes(chosen) ? chosen : (modes[0] ?? "directory");
  const environments = info.data?.environments ?? [];
  const environment = pickEnvironment(
    environments.map((e) => e.name),
    chosenEnv,
  );
  const showEnvironment = mode !== "directory" && environments.length > 0;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const form = { mode, cwd, name, repo, branch, environment };
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
    <form className="card new-session" id="new-session" onSubmit={(e) => void submit(e)}>
      <h2>New session</h2>
      {modes.length > 1 ? (
        <div className="segmented" role="radiogroup" aria-label="Workspace">
          {modes.map((m) => (
            <span key={m}>
              <input
                type="radio"
                name="workspace"
                id={`mode-${m}`}
                value={m}
                checked={mode === m}
                onChange={() => setChosen(m)}
              />
              <label htmlFor={`mode-${m}`}>{modeLabels[m]}</label>
            </span>
          ))}
        </div>
      ) : (
        <p className="kind">{modeLabels[mode]}</p>
      )}
      {showEnvironment && environments.length === 1 && <p className="kind">Environment: {environment}</p>}
      {showEnvironment && environments.length > 1 && environments.length <= 4 && (
        <div className="env-row">
          <span className="caption" id="environment-caption">
            Environment
          </span>
          <div className="segmented env" role="radiogroup" aria-labelledby="environment-caption">
            {environments.map((e) => (
              <span key={e.name} title={environmentHint(e)}>
                <input
                  type="radio"
                  name="environment"
                  id={`env-${e.name}`}
                  value={e.name}
                  checked={environment === e.name}
                  onChange={() => {
                    requestIds.current.changed();
                    setChosenEnv(e.name);
                  }}
                />
                <label htmlFor={`env-${e.name}`}>{e.name}</label>
              </span>
            ))}
          </div>
        </div>
      )}
      <div className="fields">
        {showEnvironment && environments.length > 4 && (
          <label className="wide">
            Environment
            <select
              value={environment}
              onChange={(e) => {
                requestIds.current.changed();
                setChosenEnv(e.target.value);
              }}
            >
              {environments.map((e) => (
                <option key={e.name} value={e.name} title={environmentHint(e)}>
                  {e.name}
                </option>
              ))}
            </select>
          </label>
        )}
        {mode === "directory" && (
          <label className="wide">
            Working directory
            <input value={cwd} placeholder="/absolute/path" onChange={(e) => setCwd(e.target.value)} />
          </label>
        )}
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
        <label className="wide">
          Name (optional)
          <input value={name} onChange={(e) => setName(e.target.value)} />
        </label>
      </div>
      {create.error && <p className="banner tone-error">{ConnectError.from(create.error).rawMessage}</p>}
      <div className="form-actions">
        <button type="submit" disabled={!canCreate({ mode, cwd, name, repo, branch, environment }) || create.isPending}>
          Create
        </button>
      </div>
    </form>
  );
}

function Status({ s }: { s: SessionInfo }): ReactElement {
  if (s.pendingApprovals === 0 && s.unfinishedSubmissions === 0) return <span className="faint">idle</span>;
  return (
    <>
      {s.pendingApprovals > 0 && <span className="badge tone-warn">{s.pendingApprovals} awaiting approval</span>}
      {s.unfinishedSubmissions > 0 && <span className="badge tone-link pulse">working</span>}
    </>
  );
}

export function SessionList({ onSelect }: { onSelect: (id: string) => void }): ReactElement {
  const { data, error, isPending, refetch } = useQuery(listSessions, {}, { refetchInterval: 15_000 });
  const sessions = sorted(data?.sessions ?? []);

  const newSession = () => {
    const form = document.getElementById("new-session");
    form?.scrollIntoView({ behavior: "smooth", block: "start" });
    form?.querySelector<HTMLElement>("input")?.focus({ preventScroll: true });
  };

  return (
    <div className="session-list">
      <div className="section-head">
        <h2>
          Sessions {data && <span className="muted count">{sessions.length}</span>}
        </h2>
        <div className="actions">
          <button className="secondary" onClick={() => void refetch()}>
            Refresh
          </button>
          <button onClick={newSession}>New session</button>
        </div>
      </div>
      {isPending && !error && <p className="muted">Loading</p>}
      {error && <p className="banner tone-error">{ConnectError.from(error).rawMessage}</p>}
      {data?.warnings.map((w) => (
        <p className="banner tone-warn" key={w}>
          {w}
        </p>
      ))}
      {data && sessions.length === 0 && <p className="card empty">No sessions yet. Create one below.</p>}
      {sessions.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Workspace</th>
                <th>Status</th>
                <th className="created">Created</th>
              </tr>
            </thead>
            <tbody>
              {sessions.map((s) => (
                <tr
                  key={s.id}
                  onClick={(e) => {
                    if (!(e.target as HTMLElement).closest("a")) onSelect(s.id);
                  }}
                >
                  <td>
                    <a
                      className="session-name"
                      href={`?session=${encodeURIComponent(s.id)}`}
                      onClick={(e) => {
                        e.preventDefault();
                        onSelect(s.id);
                      }}
                    >
                      {s.name || s.id}
                    </a>
                  </td>
                  <td className="workspace">
                    <span className="kind faint">{kindText(s)}</span>
                    <span className="where mono" title={where(s)}>
                      {where(s)}
                    </span>
                  </td>
                  <td>
                    <div className="status">
                      <Status s={s} />
                    </div>
                  </td>
                  <td className="created muted" title={s.createdAt ? timestampDate(s.createdAt).toLocaleString() : undefined}>
                    {age(s)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <NewSessionForm
        onCreated={(id) => {
          void refetch();
          onSelect(id);
        }}
      />
    </div>
  );
}
