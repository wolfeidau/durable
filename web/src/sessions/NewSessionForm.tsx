import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useRef, useState, type FormEvent, type ReactElement } from "react";
import { createSession, getServerInfo } from "../gen/durable/v1/durable-SessionService_connectquery";
import { environmentHint, environmentLabel } from "../lib/environments";
import { errText } from "../lib/rpc";
import { readStored, store } from "../lib/storage";
import { Segmented } from "../ui/Segmented";
import {
  buildRequest,
  canCreate,
  defaultBranch,
  modesFor,
  pickEnvironment,
  RequestIds,
  type Mode,
} from "./newSession";

const modeLabels: Record<Mode, string> = { directory: "Directory", empty: "Empty workspace", repo: "Repository" };

const cwdKey = "durable.lastCwd";

export function NewSessionForm({ onCreated }: { onCreated: (id: string) => void }): ReactElement {
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

  const selectEnvironment = (envName: string) => {
    requestIds.current.changed();
    setChosenEnv(envName);
  };

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
            onChange={selectEnvironment}
          />
        </div>
      )}
      <div className="fields">
        {showEnvironment && environments.length > 4 && (
          <label className="wide">
            Environment
            <select value={environment} onChange={(e) => selectEnvironment(e.target.value)}>
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
