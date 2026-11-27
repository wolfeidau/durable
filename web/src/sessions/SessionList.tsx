import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useMutation, useQuery, useTransport } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useRef, useState, type FormEvent, type ReactElement } from "react";
import { createSession, getServerInfo, listSessions } from "../gen/durable/v1/durable-SessionService_connectquery";
import { SessionService, type SandboxSnapshot, type SessionInfo } from "../gen/durable/v1/durable_pb";
import {
  buildRequest,
  canCreate,
  defaultBranch,
  modesFor,
  pickEnvironment,
  RequestIds,
  type Mode,
} from "./newSession";
import { environmentHint, environmentLabel } from "../lib/environments";
import { QueryStatus } from "../ui/QueryStatus";
import { Segmented } from "../ui/Segmented";
import { errText } from "../lib/rpc";
import { readStored, store } from "../lib/storage";
import {
  age,
  kindText,
  pollInterval,
  sandboxHeaderNote,
  sandboxStatusView,
  sessionWarnings,
  showSandboxColumn,
  sorted,
  where,
  workStatus,
} from "./sessions";

const modeLabels: Record<Mode, string> = { directory: "Directory", empty: "Empty workspace", repo: "Repository" };

const cwdKey = "durable.lastCwd";

function NewSessionForm({ onCreated }: { onCreated: (id: string) => void }): ReactElement {
  const [cwd, setCwd] = useState(() => readStored(cwdKey));
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
    if (mode === "directory") store(cwdKey, cwd.trim());
    setName("");
    onCreated(res.session.id);
  };

  // Until the server says which kinds it offers, the form could show the wrong fields.
  if (info.isPending) return <></>;

  return (
    <form className="card new-session" id="new-session" onSubmit={(e) => void submit(e)}>
      <h2>New session</h2>
      {modes.length > 1 ? (
        <Segmented
          aria-label="Workspace"
          options={modes.map((m) => ({ value: m, label: modeLabels[m] }))}
          value={mode}
          onChange={setChosen}
        />
      ) : (
        mode !== "directory" && <p className="kind">{modeLabels[mode]}</p>
      )}
      {showEnvironment && environments.length === 1 && <p className="kind">Environment: {environments.map(environmentLabel)}</p>}
      {showEnvironment && environments.length > 1 && environments.length <= 4 && (
        <div className="env-row">
          <span className="caption" id="environment-caption">
            Environment
          </span>
          <Segmented
            className="segmented env"
            aria-labelledby="environment-caption"
            options={environments.map((e) => ({ value: e.name, label: environmentLabel(e), title: environmentHint(e) }))}
            value={environment}
            onChange={(name) => {
              requestIds.current.changed();
              setChosenEnv(name);
            }}
          />
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
                  {environmentLabel(e)}
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
      {create.error && <p className="banner tone-error">{errText(create.error)}</p>}
      <div className="form-actions">
        <button type="submit" disabled={!canCreate({ mode, cwd, name, repo, branch, environment }) || create.isPending}>
          Create
        </button>
      </div>
    </form>
  );
}

function Status({ s }: { s: SessionInfo }): ReactElement {
  const view = workStatus(s);
  return <span className={view.className}>{view.text}</span>;
}

function Sandbox({ s, snapshot }: { s: SessionInfo; snapshot: SandboxSnapshot | undefined }): ReactElement {
  const view = sandboxStatusView(s.sandboxStatus, snapshot);
  return (
    <span className={view.className} title={view.title}>
      {view.text}
    </span>
  );
}

export function SessionList({ onSelect }: { onSelect: (id: string) => void }): ReactElement {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const { data, dataUpdatedAt, error, isPending, refetch } = useQuery(
    listSessions,
    {},
    { refetchInterval: (query) => pollInterval(query.state.data?.sandboxSnapshot) },
  );
  const [refreshErr, setRefreshErr] = useState<{ at: number; err: unknown }>();
  const sessions = sorted(data?.sessions ?? []);
  const snapshot = data?.sandboxSnapshot;
  const hasSandbox = showSandboxColumn(sessions, snapshot);
  const headerNote = sandboxHeaderNote(snapshot);
  const observedTitle = snapshot?.observedAt ? `Updated ${timestampDate(snapshot.observedAt).toLocaleString()}` : undefined;
  const warnings = sessionWarnings(data?.warnings, snapshot);
  const activeRefreshErr = refreshErr && refreshErr.at === dataUpdatedAt ? refreshErr.err : undefined;

  const refresh = async () => {
    setRefreshErr(undefined);
    try {
      const res = await createClient(SessionService, transport).listSessions({ refreshSandboxStatus: true });
      queryClient.setQueryData(
        createConnectQueryKey({ schema: listSessions, input: {}, transport, cardinality: "finite" }),
        res,
      );
    } catch (err) {
      setRefreshErr({ at: dataUpdatedAt, err });
    }
  };

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
          <button className="secondary" onClick={() => void refresh()}>
            Refresh
          </button>
          <button onClick={newSession}>New session</button>
        </div>
      </div>
      <QueryStatus isPending={isPending} error={error ?? activeRefreshErr} warnings={warnings} />
      {data && sessions.length === 0 && <p className="card empty">No sessions yet. Create one below.</p>}
      {sessions.length > 0 && (
        <div className="table-wrap">
          <table className={hasSandbox ? "has-sandbox" : undefined}>
            <thead>
              <tr>
                <th>Name</th>
                <th>Workspace</th>
                <th>Status</th>
                {hasSandbox && (
                  <th className="sandbox-col" title={observedTitle}>
                    Sandbox{headerNote && <span className="sandbox-note">{headerNote}</span>}
                  </th>
                )}
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
                      <bdi dir="ltr">{where(s)}</bdi>
                    </span>
                  </td>
                  <td>
                    <div className="status">
                      <Status s={s} />
                    </div>
                  </td>
                  {hasSandbox && (
                    <td className="sandbox">
                      <Sandbox s={s} snapshot={snapshot} />
                    </td>
                  )}
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
