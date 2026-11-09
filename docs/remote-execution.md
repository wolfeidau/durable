# Remote execution

Phase 4 of the [architecture](architecture.md#phases): the agent's tools run in a sandbox, and the
model loop, the session and every control stay outside it.

**Status: designed, not built.** Two spikes passed against a local OpenShell gateway. Their code and
`FINDINGS.md` are on the unmerged branches `spike/openshell-worker`, `spike/openshell-workspace` and
`spike/openshell-server`, kept until the slices land; the results this design depends on are copied
below. The [slices](#slices) build it properly.

## Shape

```text
 Browser               Server (host)                          OpenShell sandbox
+---------+ Connect  +---------------------------+  SDK TCP  +----------------------+
| Web UI  | -------> | durable-server            | --------> | durable-worker       |
+---------+          |  - model loop, model key  |  tunnel,  |  - read_file, grep,  |
                     |  - session.sqlite         |  connects |    list_files,       |
                     |  - hooks and approvals    |  in       |    edit_file, bash   |
                     |  - fixed tool table       |           |  - MCP servers       |
                     |  - GitHub token           |           |  - git clone of the  |
                     +---------------------------+           |    session's repo    |
                          |               |                  +----------------------+
                          v               v                        | egress: allowlist
                    Model provider   OpenShell gateway             v only, credentials
                                     (providers, policy)        GitHub   as placeholders
```

`durable chat` keeps its in-process tools. The server gains the sandbox behind a flag; local tools
stay its default until the slices land.

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
- **Credentials:** `Providers()` creates a provider from a value.
- **Client:** `gateway.NewClient` rejects a gateway in mTLS mode, which is the local default. The
  launcher passes `WithAuth(v1.NoAuth())` and `WithTLS` with the gateway directory's `mtls/` files,
  which skips that check. Raise it with OpenShell.
- **Still the CLI:** importing a provider profile with L7 rules (`openshell profile import`), because
  the SDK's profile endpoints can't carry them. The server needs the `openshell` binary for that one
  step until the SDK can.
- **Not used:** the SDK's file upload, which isn't implemented, and uploads through `exec`, which
  allocate a terminal and needed four workarounds. The workspace arrives by `git clone` instead.

### OpenShell is the first sandbox

It gives default-deny egress with rules per binary, credentials the agent only sees as placeholders,
stop and start that keep the workspace, and a Go SDK. It's Apache-2.0 and alpha. Its anonymous
telemetry is on by default and is a gateway setting, not something `durable-server` controls as a
client: the setup docs tell you to set `OPENSHELL_TELEMETRY_ENABLED=false` on the gateway.

Nothing in the worker API depends on OpenShell, so another sandbox only needs a launcher.

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
hook that runs its input, for example through `eval`, is a host escape. Slice S adds this to
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

- **Create** with the session, from a pinned image, then clone the session's repo.
- **Stop** when the server closes an idle session, which parks the sandbox through approval waits
  and keeps its workspace. **Start** when the session opens again. Nothing deletes a sandbox
  automatically; deleting a session deletes it.
- **Connect** with a new dial on each open; there's no forward to recreate.
- **Pin** the sandbox name, image digest and worker token in `sandbox.json` in the session
  directory, next to `provider.json`.
- **Fail clearly, without leaking detail.** The server logs the cause and returns a fixed message,
  as it does for a misconfigured provider, because gateway errors can name providers or `op://`
  references. A gateway that can't be reached or can't provision in time returns `UNAVAILABLE`,
  which clients retry. A configuration problem, such as a missing image, provider or profile, returns
  `FAILED_PRECONDITION`, which they don't. Neither is an internal error.

### A sandboxed session names a repo, not a directory

With tools in a sandbox, the host directory isn't where the agent works. A server started with
`--sandbox-image` creates sessions from a repo URL and a branch; the sandbox clones it at create, and
the work comes back through `git push`. Uploading a local directory isn't offered. A server without
the flag keeps directory sessions exactly as today, and so does `durable chat`.

- **Identity:** a directory session's path is a hash of its working directory and name. A repo
  session's is a hash of the canonical repo URL, the branch and the name, so `CreateSession` stays
  idempotent: the same repo, branch and name return the same session.
- **Index:** `session.json` gains `repo` and `branch` and leaves `cwd` empty. `ListSessions` shows
  `repo@branch` for those sessions.
- **UI:** the new-session form shows a repo picker when the server runs sandboxed, and the directory
  field otherwise. `ListSessions` or a server-info call tells the UI which.

### GitHub access uses a fine-grained token

The server holds one GitHub token and gives sandboxes only a placeholder for it.

- **Flags:** `serve --github-token-from-1password op://<vault>/<item>/<field>`, or the
  `DURABLE_GITHUB_TOKEN` environment variable. There's no plain flag, which would put the token in
  the process list. A dedicated variable keeps the server from picking up a broad `GITHUB_TOKEN` or
  `GH_TOKEN` from the shell. `internal/creds` names the `--api-token` flags in its errors and reads no
  environment variable, so it's generalised to take the flag and variable names first.
- **Optional:** without a token, sandboxed sessions can clone public repos but not push.
- **Fine-grained only:** the server accepts only a `github_pat_` token and refuses every other kind,
  including classic (`ghp_`) and OAuth (`gho_`, which `gh auth token` returns) tokens. At startup it
  checks the token with GitHub, so a misconfigured server fails before a session does.
- **One provider, per-session profiles.** The server keeps a single OpenShell provider holding the
  token, so the gateway holds one copy. Each session gets its own profile and policy, named from the
  session ID, that narrow the provider to the session's repo, for `git` only.
- **Two layers of scope.** The token is the ceiling: its repos and permissions, chosen when it's
  created (contents read and write, and pull requests if wanted). No session's policy can exceed it.
- **Rotation:** the server reads the token at startup and again on each sandbox create or start, and
  updates the provider's credential in place whenever the value changes, so a rotated token takes
  effect without a restart. Whether a running sandbox picks up the update before its next start is
  untested. Fine-grained tokens expire, a year at most; after expiry, pushes fail with a 401 until
  the token is rotated.
- **Cleanup:** deleting a session deletes its sandbox, then its profile. The gateway refuses to
  delete a profile while its sandbox still exists, so the server waits for the sandbox to go first.
- **Later:** a GitHub App with short-lived installation tokens per repo, when durable is hosted.

### Instructions and skills come from the workspace

The project `AGENTS.md` and project skills live in the cloned repo, so the worker reads them once
when the session is created, and the server pins them as it does today. `~/.durable/AGENTS.md` and
user skills stay on the server. Reading through the worker keeps the existing rules: symlinks that
leave the repository are refused, and the content is untrusted. The spikes still read them from the
host.

### MCP servers run in the sandbox

stdio MCP servers start inside the sandbox, beside the worker, and their credentials come from
OpenShell providers. The tool list stays pinned per session. Not yet spiked.

## Slices

Each slice is one PR. The spike branches are reference, not a base.

| Slice | Scope | Depends on |
|---|---|---|
| **W. Worker** | `worker.proto`, `internal/worker`, `cmd/durable-worker`, the remote `Tool` adapter with the fixed table, and the local subprocess launcher. The exported delta helpers. Tests run without OpenShell: deltas, cancel, recovery, and a worker that lies about metadata | |
| **L. OpenShell launcher** | The SDK client with the mTLS workaround, the lifecycle, the TCP dial, and the worker image build. Tests against a gateway are skipped without one | W |
| **S. Server wiring** | `serve --sandbox-image`, `sandbox.json`, the tool source in `Assemble`, stop on idle, the `UNAVAILABLE` and `FAILED_PRECONDITION` errors, `durable-server sandbox-delete`, and the hooks note. Real-binary tests for the flag: approval, a call in the sandbox, stop on idle and start on reopen | L |
| **G. GitHub** | The token flags and check, the provider and per-session profile and policy, cleanup, and the clone at create. Real-binary tests for both token sources, and that classic and OAuth tokens are refused | S |
| **R. Repo sessions** | `CreateSession(repo, branch)`, repo session identity and `session.json` fields, and the UI's repo picker when the server is sandboxed. Real-binary tests: an idempotent create, and directory sessions unchanged | G |
| **I. Instructions** | `AGENTS.md` and project skills read through the worker and pinned | S |
| **M. MCP** | stdio MCP servers inside the sandbox | S |

## Open questions

- **Before G:** should `git push` go through durable's approval flow? Today the policy allows it
  without a prompt.
- **Before L:** who builds the worker image, and how is it pinned and published: one image, or one
  per repository with its toolchain? [Issue #9](https://github.com/wolfeidau/durable/issues/9)
  rejected containers for local use because an image has to match the project's toolchain, and that
  cost now applies here.
- **Before S:** what does a gateway restart do to running sandboxes and their workers? The spikes
  didn't test it.
- **Later:** one sandbox per session, or one per repository shared across sessions?
- **Later:** a per-session egress policy beyond GitHub, and how a person approves a new rule.
- **Later:** a cancelled call's result takes about 2 s to be stored. Find out why before it matters.
