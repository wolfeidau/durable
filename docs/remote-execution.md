# Remote execution

Phase 4 of the [architecture](architecture.md#phases): the agent's tools run in a sandbox, and the
model loop, the session and every control stay outside it.

**Status: built** in #88 to #96, against a local [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell)
gateway. Two spikes proved the design first, and the results it depends on are copied below. Every
[slice](#slices) is done except more git hosts.

## Shape

```text
 Browser               Server (host)                          OpenShell sandbox
+---------+ Connect  +---------------------------+  SDK TCP  +----------------------+
| Web UI  | -------> | durable-server            | --------> | durable-worker       |
+---------+          |  - model loop, model key  |  tunnel,  |  - read_file, grep,  |
                     |  - session.sqlite         |  connects |    list_files,       |
                     |  - hooks and approvals    |  in       |    edit_file, bash   |
                     |  - fixed tool table       |           |  - MCP servers       |
                     |  - git host tokens        |           |  - workspace: empty  |
                     +---------------------------+           |    or a repo clone   |
                          |               |                  +----------------------+
                          v               v                        | egress: allowlist
                    Model provider   OpenShell gateway             v only, credentials
                                     (providers, policy)      git host   as placeholders
```

`durable chat` keeps its in-process tools. The server gains the sandbox behind a flag, and local
tools stay its default.

## What the spikes proved

| | Result |
|---|---|
| Connect | The SDK's TCP tunnel carries Connect over HTTP/2: no host port, no `ssh` child, and after `sandbox start` the next call just dials again |
| Latency | About 1 ms per tool call at p50, the same as the CLI's forward |
| Streaming | `bash` output arrives as it's written, through the existing delta path |
| Cancel | A cancelled `sleep 30` stops within 10 ms and is never re-run |
| Recovery | A dropped connection doesn't stop a call; `Result` recovers it on the next dial |
| Untrusted metadata | A worker that claims `bash` is read-only, or adds a tool, is ignored |
| Workspace | The sandbox cloned a private repo, edited it through the remote tools and pushed; the host saw the same commit |
| Credentials | The agent's environment held only a placeholder for the GitHub token. `curl` to other hosts was denied, and git was limited to one repo (another repo returned 403) |
| Server | A session's `bash` call parked for approval, ran once in the sandbox as uid 1500 and streamed its output. The sandbox stopped about 4 s after the session idled and started again on reopen in about 0.6 s |

## Decisions

### Tools run in the sandbox, and the loop stays outside

Approvals, hooks and `session.sqlite` are security controls, and a control has to sit outside what
it controls. With the loop inside, `bash` runs as the same user as the harness and could write an
approved row, edit the log or kill a hook. Outside, the loop never executes model output: it routes
it to the provider, to hooks and approvals, and to the worker, and what comes back is data.

This also changes what [hooks](hooks.md) are: with tools in the sandbox, `bash` can no longer edit
`~/.durable/hooks.json`, so hooks become a boundary.

### The server connects in

This replaces [workers dial out](architecture.md#workers-dial-out). The server creates the sandbox,
starts the worker in it and connects to it, so the sandbox has no route to the server and its egress
lists only what the tools need. A compromised sandbox can't reach the API that approves commands.

A launcher creates, starts, stops and deletes a sandbox and returns a connection to its worker:

- **OpenShell,** through its Go SDK.
- **Local subprocess** on loopback, for tests and development.

The worker serves Connect over unencrypted HTTP/2 on `127.0.0.1` inside the sandbox, and runs each
call detached from the connection.

### The OpenShell launcher uses the Go SDK

[`github.com/ai-openshell/sdk-go`](https://github.com/ai-openshell/sdk-go), pinned to the gateway's
version (`v0.1.7` today). It lives in its own package, which only `durable-server` imports, so
`durable chat` and the library don't link it. It adds `grpc`, `genproto` and a few `x/` modules; the
worker binary links none of it.

- **Connection:** `TCP().Forward` returns a byte stream to a port inside the sandbox over the
  gateway's authenticated gRPC connection. It is the `http.Transport`'s dial function. Its stream
  context must not be the dial's context, which ends when the dial returns.
- **Lifecycle:** `Sandboxes()` `Create`, `WaitReady`, `Stop`, `Start`, `WaitStopped`, `Delete`.
- **Credentials:** `Providers()` creates a provider from a value, and `Providers().Profiles()` imports the profile it
  uses.
- **Client:** `gateway.NewClient` rejects a gateway in mTLS mode, which is the local default. The
  launcher passes `WithAuth(v1.NoAuth())` and `WithTLS` with the gateway directory's `mtls/` files,
  which skips that check. Raise it with OpenShell.
- **Not used:** the SDK's file upload, which isn't implemented, and uploads through `exec`, which
  allocate a terminal and needed four workarounds. The workspace arrives by `git clone` instead.

### OpenShell is the first sandbox

It gives default-deny egress with rules per binary, credentials the agent only sees as placeholders,
stop and start that keep the workspace, and a Go SDK. It's Apache-2.0 and alpha. Its anonymous
telemetry is on by default and is a gateway setting, not something `durable-server` controls as a
client: the setup docs tell you to set `OPENSHELL_TELEMETRY_ENABLED=false` on the gateway.

Nothing in the worker API depends on OpenShell, so another sandbox only needs a launcher.

### The worker image is built locally

`mise run image:worker` builds `durable-worker:dev` from `docker/worker.Dockerfile`. There is no
registry and no published image, because an image has to match the project's toolchain
([issue #9](https://github.com/wolfeidau/durable/issues/9)) and one shared image can't.

The image contract:

- The worker is at `/usr/local/bin/durable-worker`.
- A user `app` with uid and gid 1500 runs it, because OpenShell drops agent processes to that user.
- `/sandbox` is owned by that user and is the working directory.
- `bash` and `git` are present.

For a project with its own toolchain, start with `FROM durable-worker:dev`, switch to root to
install the tools, and switch back to `app`. Copying the worker binary into another image works only
if that image has glibc at least as new as Debian 13's, because the worker links SQLite through cgo;
musl images such as Alpine can't run it.

The gateway runs images from its own container runtime, so a locally built tag is visible to a local
gateway only.

### The server owns tool metadata

The worker is untrusted. Whether a tool is read-only decides approval, and its replay decides
whether it re-runs, so both come from the server's fixed table. The server also owns each tool's
name, description and schema, because they go into the system prompt. `ListTools` only checks that
the worker serves the tools the server expects, in the same order with the same schemas; a mismatch
fails the session open. Unknown tools are ignored. This is the rule MCP server hints already follow.

### Worker output is hostile input

Results and deltas are capped and must be valid UTF-8, and a malformed response is an error result.
`PostToolUse` hooks run on the server, before the result is stored.

### Hooks get untrusted input

Hooks still run on the host, as you. Their input now comes from a sandbox the agent controls, so a
hook that runs its input, for example through `eval`, is a host escape. This is also in
[hooks](hooks.md).

### Worker API

`durable.worker.v1.ToolService`, keyed by call ID so every call is idempotent:

| RPC | Purpose |
|---|---|
| `Info` | Hostname, user, working directory and environment, without the worker's token |
| `ListTools` | What the worker serves, for the server to check |
| `Execute(call_id, name, input, stream_output)` | Server stream: output deltas, then the result. A known call ID returns its result without running again |
| `Result(call_id)` | The result of a finished call, for recovery |
| `Cancel(call_id)` | Stops a running call |
| `Stdio` | Bidirectional stream that runs one process and pipes its stdin and output. The first request starts it, and ending the stream kills its process group. It runs MCP servers and knows nothing of MCP |

- **A dropped connection doesn't stop a call.** The server recovers the result with `Result`. User
  cancel and lease loss send `Cancel`.
- **`stream_output` is false when a `PostToolUse` hook matches.** The server decides before the
  call, so live output never bypasses a hook. The library exports `WithToolOutputSink` and
  `ToolOutputEnabled` beside `WithoutToolOutput` for the worker and the adapter.
- **A cancelled code is cancellation only when the server's own context has ended.** Connect sends a
  request's deadline to the worker, which can answer `canceled` first; otherwise that's a tool error.

### The worker token isn't secret from the agent

The worker requires a bearer token. The agent can read it from the worker's environment, because
`bash` runs as the same user: the sandbox has no capabilities and no-new-privileges, so `sudo`,
`setpriv`, setuid binaries, `su`, `hidepid` and user namespaces all fail. A filesystem policy that
hides `/proc` blocks the read, but also breaks `ps` and anything else that reads `/proc`, so it
isn't the default.

That's acceptable: the agent can already run the tools, the token only opens the worker, and the
sandbox has no route to the server. The server stores the token in the session directory, mode
`0600`, so a server restart can reach the session's sandbox again.

### Results don't survive a worker restart

The worker keeps results in memory. After a restart, `Result` finds nothing, and recovery follows the
existing rules: a `ReplaySafe` call runs again, and a `ReplayNever` call records an unknown outcome.
Persisting results would add a store the agent can write to, for a case the harness already handles.

### One sandbox per session

- **Create** with the session, from a pinned image, then clone the session's repo if it has one.
- **Stop** when the server closes an idle session, which parks the sandbox through approval waits
  and keeps its workspace. **Start** when the session opens again. Nothing deletes a sandbox
  automatically; deleting a session deletes it.
- **Connect** with a new dial on each open; there's no forward to recreate.
- **Pin** the sandbox name, image and worker token in `sandbox.json` in the session directory, next
  to `provider.json`, before the sandbox is created, so a failed create leaves its token on disk and
  the next open creates again. The name is `ds-` and 16 random hex characters, never derived from
  the session. The image is used only at create, because stop and start keep the container, so a
  rebuilt tag such as `durable-worker:dev` affects only a new sandbox. A new sandbox has a fresh
  workspace, and `ListTools` rejects a worker whose catalog doesn't match.
- **A failed first open deletes the sandbox and keeps the pin,** so a retry creates it fresh from the
  current image instead of reusing a stale one.
- **A session with `sandbox.json` always runs in its sandbox,** even on a server started without
  `--sandbox-image`. A session without one stays local, and `durable chat` refuses a sandboxed
  session rather than run its tools on the host.
- **A gateway restart** leaves a sandbox in `Error` with the `Ready` reason `StartFailed`
  ([#90](https://github.com/wolfeidau/durable/issues/90),
  [NVIDIA/OpenShell#4910](https://github.com/NVIDIA/OpenShell/issues/4910)). Open recovers it with
  a stop, then a start. Any other `Error`, such as `MainProcessFailed`, is configuration.
- **`durable-server sandbox-delete <session-id>`** deletes the sandbox and keeps `sandbox.json`, so
  the next open creates a fresh, empty sandbox with the same pin, and a repo session clones its
  branch again on that open. It is the escape hatch when
  OpenShell can't recover a sandbox. It takes the server lock, so it is refused while `serve` runs.
- **Never restart the worker.** A worker that exits leaves the sandbox in `Error`, which is a
  configuration failure, and a failed create deletes its sandbox, so no sandbox is left with a token
  nobody kept. On OpenShell v0.1.7 the `OnFailure` restart policy crash-loops in `Starting`, where
  the sandbox can be neither stopped nor deleted.
- **The worker token is in the sandbox spec,** so anyone who can read the gateway can read it. That
  is the same principal that runs the server; the token keeps other local processes out, not the
  gateway's operator.
- **Fail clearly, without leaking detail.** The server logs the cause and returns a fixed message,
  as it does for a misconfigured provider, because gateway errors can name providers or `op://`
  references. A gateway that can't be reached or can't provision in time returns `UNAVAILABLE`,
  which clients retry. A configuration problem, such as a missing image, provider or profile, returns
  `FAILED_PRECONDITION`, which they don't. Neither is an internal error.

### Running it

Prerequisites: an OpenShell gateway, and the worker image from `mise run image:worker`.

```sh
durable-server --sandbox-image durable-worker:dev
```

- `--sandbox-image <ref>` runs the built-in tools of new sessions in an OpenShell sandbox from the
  image. Existing sessions keep their pin. The server connects to the gateway on first use, so
  without the flag and without sandboxed sessions it never contacts one.
- A server with the flag creates empty and repo sessions: `CreateSession` takes `workspace: EMPTY` and a
  `request_id`, or `workspace: REPO` with a `repo` and `branch`, and refuses a directory with
  `FAILED_PRECONDITION`. Without the flag it refuses both the same way. `GetServerInfo` lists the kinds the server accepts.
- `--git-host-token <kind>=<source>` gives the server a git host token; see [git host tokens](#git-host-tokens).
- `durable-server sandbox-delete <session-id>` deletes a session's sandbox; see above.
- `mise run test:openshell` runs the gateway tests.

#### Git host tokens

```sh
durable-server --sandbox-image durable-worker:dev --git-host-token github=op://<vault>/<item>/<field>
```

- `--git-host-token github=<source>` is repeatable, one per host kind. The source is an `op://`
  reference or `gh`, which runs `gh auth token`. Without the flag, `DURABLE_GITHUB_TOKEN` is read if it
  is set. `GITHUB_TOKEN` and `GH_TOKEN` are never read. A token without `--sandbox-image` is an error.
- Use a fine-grained token with contents read and write on the repos durable should reach. A classic
  or `gh` token usually reaches every repo you can.
- At startup the server resolves each token and checks it with `GET /user`, then logs the login, the
  token's kind and its scopes, never the token. A bad token fails startup. The server reads the source again
  whenever it creates or starts a repo sandbox, so a rotated token takes effect without a restart.
- A repo session on a host with no token gets anonymous access; see [repo sessions](#a-sandboxed-sessions-workspace-is-empty-or-a-repo).

A repo session, created through the API:

```sh
# workspace REPO, repo github.com/owner/name, branch main; the UI form has the same fields
durable-server --sandbox-image durable-worker:dev --git-host-token github=gh
```

### A sandboxed session's workspace is empty or a repo

With tools in a sandbox, the host directory isn't where the agent works. A server started with
`--sandbox-image` creates sessions with one of two workspaces. A server without the flag keeps
directory sessions exactly as today, and so does `durable chat`. Uploading a local directory isn't
offered.

| | Empty (scratch) | Repo |
|---|---|---|
| Workspace | An empty `/sandbox` | A clone of a repo URL and branch, made on open whenever the workspace is empty |
| Work comes back through | Tool results only | `git push` |
| Needs a git host | No | Yes, through its [integration](#git-hosts-are-pluggable) |
| Session key | A hash of the client's `request_id` | `sha256("repo\0" + repo + "\0" + branch + "\0" + name)`, with the canonical repo |
| `session.json` | `workspace: "empty"` | `workspace: "repo"`, `repo`, `branch` |
| `ListSessions` shows | The name | `repo@branch` |

- **Idempotent create.** A repo session's key comes from what names it, so the same repo, branch and
  name return the same session. An empty session has nothing to name it by, so the client sends a
  `request_id`, and the server returns the session it already made for that ID.
- **The empty session's ID comes from the `request_id`.** It is the first 16 hex characters of
  `sha256("empty\x00" + request_id)`. A retry lands on the same directory by construction, with no
  index to scan and no crash window between making the session and recording the ID. A directory
  key starts with `/`, so the two kinds can't share a preimage. A retry returns the existing
  session whatever name it sends.
- **The repo is a text field:** a URL or `github.com/owner/name`, parsed by the git host and stored in
  its canonical form. The branch is required, and the name defaults to `default`. The UI prefills `main`.
- **The clone runs on open whenever the workspace is empty,** so it happens at create and again after
  `sandbox-delete`. A workspace that has files is never cloned over. Git clones into a staging
  directory, `.durable-clone`, and the files move into place once it succeeds, so a clone that is
  killed partway is removed and retried on the next open. The clone is synchronous inside
  `CreateSession`. A failed clone returns `FAILED_PRECONDITION` with git's last output line, cleaned of
  control characters and capped at 200 bytes. A new session's sandbox is deleted, so a retry clones
  afresh; the pin and `session.json` stay.
- **No token means anonymous access.** The session's access has a rule with no provider, so it clones
  public repos only and `git push` fails. Repo access is fixed at create, so a token added later
  doesn't upgrade an existing session. The reverse holds too: a session created with a token needs
  one on every later open, and fails to open without it instead of silently becoming anonymous.
  `git push` needs no approval prompt of its own.
- **Existing sessions keep their pin.** They keep working whatever the flag, including directory
  sessions with a `sandbox.json` and repo sessions, whose clone URL is in the pin.
- **No host directory, so no project context.** An empty session pins the user's
  `~/.durable/AGENTS.md` and user skills only, because it has no repo to read. A repo session also
  reads its project files from the clone, as
  [below](#instructions-and-skills-come-from-the-workspace). Its hooks run in a temp folder with an
  empty `cwd`.
- **Both kinds persist.** Either workspace survives stop and start, and is lost only when the
  session is deleted.
- **UI:** when the server runs sandboxed, the new-session form offers an empty workspace or a repo;
  otherwise it shows the directory field. `GetServerInfo` tells the UI which, and `ListSessions`
  reports each session's `workspace`.

### Git hosts are pluggable

GitHub is the first git host integration, not a special case. Each integration implements one
interface, which runs only on the server and produces data for the launcher:

| Responsibility | GitHub |
|---|---|
| Parse and canonicalise a repo URL, including a self-hosted host | `github.com/<owner>/<name>` |
| Check a token at startup, and report its type and scope | `GET /user`, and the `X-OAuth-Scopes` header |
| Request rules scoped to one repo, as neutral data | git smart-HTTP paths for that repo |
| Egress policy entries, per binary | `github.com:443` for `git` and `git-remote-https` |
| Git configuration inside the sandbox | A credential helper with user `x-access-token` |

GitLab, Bitbucket, Gitea or Forgejo, and self-hosted GitHub Enterprise or GitLab each add one
implementation. Whatever the host, the sandbox only sees placeholders.

- **Configuration:** a repeatable `serve --git-host-token <host>=<source>`, where the source is a
  1Password reference (`op://<vault>/<item>/<field>`) or a host's own CLI (`gh` for GitHub), or one
  environment variable per host, such as `DURABLE_GITHUB_TOKEN`. A self-hosted instance also names its
  address, for example `--git-host github-enterprise=ghe.example.com`. There's no plain token flag,
  which would put the token in the process list, and a dedicated variable keeps the server from
  picking up a broad `GITHUB_TOKEN` or `GH_TOKEN` from the shell. `internal/creds.Resolve`
  takes the flag and variable names, and its errors never include the value or a command's stderr.
- **Optional:** empty sessions need no git host at all. A repo session on a host with no token should
  still clone public repos, so its access has a rule with no provider and the launcher creates no
  provider or credential binding for it.
- **One profile and one provider per host.** The profile, `durable-github`, holds the credential and no
  endpoints. The server imports it through the SDK when the gateway doesn't have it, and ensures the
  provider of the same name, which holds the token. Both live in the launcher's workspace. Each repo
  session's policy has a `durable-git` rule whose endpoint lists only that repo's smart-HTTP paths, for
  `git` and `git-remote-https` only, and binds the provider with `CredentialBinding`. Without the
  binding the sandbox gets no placeholder and auth fails. The sandbox lists the provider, and sees
  only a placeholder in `GITHUB_TOKEN`. Several repos merge into one endpoint.
- **Two layers of scope.** The token is the ceiling: its repos and permissions, chosen when it's
  created. No session's policy can exceed it.
- **Rotation:** the server reads each token from its source at startup and again on each sandbox
  create or start, and updates the provider's credential in place whenever the value changes, so a
  rotated token takes effect without a restart. Whether a running sandbox picks up the update before its next start is
  untested. After a token expires, pushes fail until it's rotated.
- **Cleanup:** deleting a session deletes its sandbox. The provider and profile are shared, so they
  stay. To remove them, delete the provider, then the profile, once no sandbox uses them, for example
  `openshell provider delete durable-github` and then `openshell profile delete durable-github`.

#### GitHub

- **Any token kind.** The integration accepts fine-grained (`github_pat_`), classic (`ghp_`) and
  OAuth (`gho_`) tokens. At startup it checks the token with GitHub, so a misconfigured server fails
  before a session does, and logs the token's type and granted scopes.
- **From the `gh` CLI:** `--git-host-token github=gh` runs `gh auth token`, with `--hostname` for
  GitHub Enterprise, and never logs the value. It's the quickest start, and `gh` keeps the token
  fresh.
- **Recommended: a fine-grained token** with contents read and write, and pull requests if wanted, on
  the repos durable should reach. A `gh` or classic token usually reaches every repo you can, so a
  missing or wrong per-session policy, or a leak from the server or gateway, exposes all of them. The
  per-session policy still limits each sandbox to its repo whichever kind you use. Fine-grained
  tokens expire, a year at most.
- **Later:** a GitHub App with short-lived installation tokens per repo, when durable is hosted.

#### What each integration needs from OpenShell

The gateway has no built-in profiles, so the server imports one credential-only profile per host, and
ensures one provider that holds the token, both through the SDK. The request rules live in each
sandbox's policy, so no CLI and no per-session profile are needed. One gap is worth raising with
OpenShell:

- **`gateway.NewClient` rejects mTLS gateways,** which is the local default, so the launcher works
  around it.

### Instructions and skills come from the workspace

A repo session reads the project's context from its clone, through the worker. The worker has three
read-only RPCs for it, `ReadFile`, `ReadDir` and `Stat`. Each opens the workspace through `os.Root`,
so a symlink that leaves it is refused, and each accepts only a relative path that stays inside it.
They are not model tools.

- **What is read:** `AGENTS.md` at the workspace root, and skills under `.durable/skills` and
  `.agents/skills`, in that order. There are no parent directories, because the clone root is the
  repo root. The budgets, the binary and truncation checks, and the warnings are the same as for a
  local session.
- **Pinned at create:** the text of `AGENTS.md` goes in `instructions.md`, and the skills' metadata
  goes in `skills.json`, as they do today. The user's `~/.durable/AGENTS.md` and user skills stay on
  the server and are added to the same pins. A reopen reads the pins and discovers nothing.
- **Skills are read live:** the `skill` tool reads `SKILL.md` and lists a skill's files when it is
  called, through the worker, as a local session reads the live files. A skill from the clone is
  marked `sandbox` in `skills.json`, and its directory is relative to the workspace. A reopen gives
  the tool the new worker, so a skill still works after the sandbox is stopped or deleted and cloned
  again.
- **Failures:** a missing file is "none". A file that is a symlink out of the repo, binary or not a
  regular file is skipped with a warning. A failure of the worker itself fails the create, and a new
  sandbox is discarded as for a failed clone.
- **The worker image must match the server.** A worker built before these RPCs answers
  `Unimplemented`, and the error says to rebuild the image. Only new repo sessions read the
  workspace, so only they are affected. An existing session keeps its pins, which are empty for a
  repo session created before this.
- **Untrusted:** the repo is the user's own choice, but its text can say anything, so the same rule
  applies as for a local project: instructions don't override the user's direct requests.

An empty workspace has no project context, and directory sessions read the host as before.

### MCP servers run in the sandbox

The server keeps the MCP client: discovery, the per-session pin, naming, caps, timeouts, approval and
`ReplayNever` are the same as for a local session, and server hints still change nothing. The worker
is only a pipe.

**The sandbox is the trust boundary, not the MCP server.** The agent and the session's MCP servers
share one sandbox, one user and one egress allowlist, so the agent can reach anything its MCP servers
can: their credentials and their hosts. That is an accepted trade-off. What matters is what the
sandbox can't do: touch the host, reach the server's API, or reach any host outside the allowlist.
Giving each MCP server its own sandbox would separate its credentials and egress from the agent; see
[open questions](#open-questions).

- **stdio servers run beside the worker** through the `Stdio` RPC. The server wraps the stream in an
  MCP transport. The process gets the worker's environment (without its token) plus the server's
  configured `env`, including `${VAR}` values the host expanded. It runs in its own process group in
  `/sandbox` when `useWorkspaceCwd` is set, otherwise in a temp folder. Ending the stream closes
  stdin, then sends SIGTERM and SIGKILL after 2 seconds each, so closing a session or stopping the
  sandbox ends its servers.
- **HTTP servers run through a bridge.** `durable-worker mcp-http --url <url>` is started the same
  way, with the headers in `DURABLE_MCP_HEADERS`, which it reads and then unsets. It connects to the
  remote server, serves the same tools over stdio, and forwards calls, so the traffic leaves from the
  sandbox. Headers go only to the URL's origin.
- **Egress follows the config.** The first pin records the host and port of each HTTP server (443 or
  80 by scheme) in `sandbox.json`, and create gives the sandbox one allow rule for those endpoints,
  scoped to `/usr/local/bin/durable-worker`. A sandbox created with no policy has no egress, so
  without HTTP servers nothing is added. Other hosts stay denied, for `bash` and for a stdio server's
  own network use, until there is a per-session egress policy. The rule is not updated when
  `mcp.json` changes, because the pin is written once.
- **The rule reaches every process the worker starts.** OpenShell matches it against a process's
  ancestors, and the worker starts every tool, so an approved `bash` call can reach the HTTP
  servers' hosts too (`curl` from `bash` got a response from the configured host and was refused
  elsewhere). With the headers also readable, the agent can call those servers directly, outside
  MCP approval, through any approved `bash` command. This follows from the shared trust boundary
  above; providers, or a sandbox per MCP server, would close it.
- **`env` and headers are visible to the agent.** It runs as the same user, so it can read them.
  OAuth and OpenShell providers are the way out later.
- **Gateway behaviour the launcher depends on** (probed on OpenShell v0.1.7): a policy keeps the
  default filesystem and process settings (user 1500, writable `/sandbox`), but drops the default
  Landlock `best_effort`, so the launcher sets it again. The rule's binary is matched by exact path
  against the process and its ancestors: a copy of `curl` started outside the worker was refused.
  TLS works because the sandbox trusts OpenShell's CA through `SSL_CERT_FILE`, which Go honours.
- **The sandbox's output is hostile input:** messages are capped at 4 MiB and stderr at 1 MiB per
  connection, written to `mcp-<server>.log`.
- **The base image has no `npx` or `uvx`.** Add them with `FROM durable-worker:dev`.
- **Sessions created before this slice stay without MCP tools,** because their pin is `[]` and pins
  never change. Their `sandbox.json` has no egress either.

## Slices

Each slice is one PR.

| Slice | Scope | Depends on |
|---|---|---|
| **W. Worker** (done) | `worker.proto`, `internal/worker`, `cmd/durable-worker`, the remote `Tool` adapter with the fixed table, and the local subprocess launcher. The exported delta helpers. Tests run without OpenShell: deltas, cancel, recovery, and a worker that lies about metadata | |
| **L. OpenShell launcher** (done) | The SDK client with the mTLS workaround, the lifecycle, the TCP dial, and the worker image build. Tests against a gateway are skipped without one | W |
| **S. Server wiring** (done) | `serve --sandbox-image`, `sandbox.json`, the tool source in `Assemble`, stop on idle, the `UNAVAILABLE` and `FAILED_PRECONDITION` errors, `durable-server sandbox-delete`, and the hooks note. Real-binary tests for the flag: approval, a call in the sandbox, stop on idle and start on reopen | L |
| **E. Empty sessions** (done) | Sandboxed sessions with an empty workspace: their key, `session.json` fields, create by `request_id`, and the UI's choice of workspace. The first sandboxed agent you can use, with no git host. Real-binary tests: a retried create returns the same session, and directory sessions unchanged | S |
| **G. Git hosts and GitHub** (done) | The git host interface, the token flags and environment variables, one provider per host, per-session repo access in the sandbox policy, and GitHub as the first integration. Real-binary tests for each token source (1Password, environment variable and `gh`), and the startup check | S |
| **R. Repo sessions** (done) | `CreateSession` with a repo and branch through the git host interface, the clone at create, and repo session identity. The repo picker is deferred; the repo is a text field. Real-binary tests: an idempotent create, and a clone and push | E, G |
| **H. More git hosts** | One slice per host, such as GitLab or GitHub Enterprise: its implementation | G |
| **I. Instructions** (done) | `AGENTS.md` and project skills read through the worker and pinned | S |
| **M. MCP** (done) | MCP servers inside the sandbox: the `Stdio` RPC, the HTTP bridge, and egress for HTTP servers. Tests: a stdio server through the worker, the bridge against an HTTP server, the kill of the process group, and the launcher's policy | S |

## Open questions

- **Resolved in G, `git push`:** it is allowed without an approval prompt. The limits are the token's
  scope and the session's per-repo rule in its sandbox policy.
- **Resolved in G, profile import:** the `openshell` CLI isn't needed. The profile has no endpoints, so
  the Go SDK imports it, and the request rules live in the sandbox policy. mTLS support in
  `gateway.NewClient` is still missing; see [the launcher](#the-openshell-launcher-uses-the-go-sdk).
- **Resolved in S, gateway restart:** a restart leaves sandboxes in `Error` with reason
  `StartFailed`, which open recovers with a stop and a start. See
  [#90](https://github.com/wolfeidau/durable/issues/90) and
  [NVIDIA/OpenShell#4910](https://github.com/NVIDIA/OpenShell/issues/4910).
- **Resolved in S, local image pinning:** `sandbox.json` pins the reference as given, and the image
  matters only at create. See [one sandbox per session](#one-sandbox-per-session).
- **Later:** does a running sandbox pick up a rotated provider credential before its next start?
- **Later:** one sandbox per session, or one per repository shared across sessions?
- **Later:** a per-session egress policy beyond GitHub, and how a person approves a new rule.
- **Later:** run each MCP server in its own sandbox, so its credentials and egress aren't shared
  with the agent. Today they share the session's sandbox; see
  [MCP servers run in the sandbox](#mcp-servers-run-in-the-sandbox).
- **Later:** a cancelled call's result takes about 2 s to be stored. Find out why before it matters.
