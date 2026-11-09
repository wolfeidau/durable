# Remote execution

Phase 4 of the [architecture](architecture.md#phases): the agent's tools run in a sandbox, and the
model loop, the session and every control stay outside it.

**Status: draft.** The first spike passed (`spike/openshell-worker`, see its `FINDINGS.md`). This
draft records the decisions so far and what the second spike must prove. The slice plan follows the
second spike.

## Shape

```text
 Browser                Server (host)                         OpenShell sandbox
+---------+  Connect  +---------------------------+  SDK      +----------------------+
| Web UI  | --------> | durable-server            | --------> | durable-worker       |
+---------+           |  - model loop, model key  |  connects |  - read_file, grep,  |
                      |  - session.sqlite         |  in       |    list_files,       |
                      |  - hooks and approvals    |           |    edit_file, bash   |
                      |  - fixed tool table       |           |  - MCP servers       |
                      +---------------------------+           |  - workspace         |
                                   |                          |  - egress allowlist  |
                                   v                          |    only              |
                             Model provider                   +----------------------+
```

`durable chat` keeps its in-process tools. The server gains the sandbox; local tools stay its
default until the sandbox path is proven.

## Decisions

### Tools run in the sandbox, and the loop stays outside

Approvals, hooks and `session.sqlite` are security controls, and a control has to sit outside what
it controls. With the loop inside, `bash` runs as the same user as the harness and could write an
approved row, edit the log or kill a hook. Outside, the loop never executes model output: it routes
it to the provider, to hooks and approvals, and to the worker, and what comes back is data.

This holds under the rules below. It also changes what [hooks](hooks.md) are: with tools in the
sandbox, `bash` can no longer edit `~/.durable/hooks.json`, so hooks become a boundary.

### The server connects in

This replaces [workers dial out](architecture.md#workers-dial-out). The server starts the worker and
connects to it, as go-plugin does, so the sandbox has no route to the server and its egress lists
only what the tools need. A compromised sandbox can't reach the API that approves commands.

A launcher returns a connection to the worker:

- **OpenShell,** through its Go SDK. See below.
- **Local subprocess** on loopback, for tests.

The worker serves Connect over unencrypted HTTP/2 and runs each call detached from the connection,
so a dropped connection never stops a call.

### The OpenShell launcher uses the Go SDK

The launcher talks to the gateway through
[`github.com/ai-openshell/sdk-go`](https://github.com/ai-openshell/sdk-go), pinned to the gateway's
version (`v0.1.7` today). The module moved there from `github.com/NVIDIA/OpenShell/sdk/go`, which is
deprecated. It gives typed calls and errors, authenticates with the gateway's existing mTLS setup,
and removes the `openshell` binary as a runtime dependency. The CLI is used only for what the SDK
can't do, and the second spike finds out what that is.

Its dependencies are small: `grpc`, `x/oauth2`, `coder/websocket` and `genproto`, beside `protobuf`,
`x/sync` and `testify`, which durable already has. None needs cgo. It lives in its own package, which
only `durable-server` imports, so `durable chat` and the library don't link it.

How the connection reaches the worker depends on what the SDK offers:

- **A port forward, if the SDK has one.** The worker listens on `127.0.0.1` inside the sandbox, as
  in the first spike.
- **Otherwise, exec with a relay.** The worker listens on a Unix socket. For each connection the
  launcher runs `durable-worker connect` through the SDK's exec, which only relays stdin and stdout
  to that socket, and Connect runs over the stream with yamux. A dropped stream kills only the relay.
  This needs no host port, no port per session and no forward to recreate after `sandbox start`.

The first spike used the CLI's forward. It works, but it's an `ssh -L` child process with no
documented auth on its host port, it hangs a caller that pipes its output, and `sandbox start`
doesn't restore it.

The worker requires a bearer token on a host port, to keep other host processes off it. Over exec
only the gateway's authenticated stream reaches the socket, so the token becomes optional. Either
way it isn't a secret from the agent, which can read the worker's environment.

### OpenShell is the first sandbox

It gives default-deny egress with rules per binary, credentials the agent only sees as placeholders,
stop and start that keep the workspace, and a Go SDK, which the launcher uses. It's Apache-2.0 and alpha, and its telemetry
is on by default: the server's setup turns it off with `OPENSHELL_TELEMETRY_ENABLED=false`.

Nothing in the worker API depends on OpenShell, so another sandbox only needs a launcher.

### The server owns tool metadata

The worker is untrusted. Whether a tool is read-only decides approval, and its replay decides
whether it re-runs, so both come from the server's fixed table. The server also owns each tool's
name, description and schema, because they go into the system prompt. `ListTools` only checks that
the worker serves the tools the server expects, with matching schemas; a mismatch fails the session
open. Unknown tools are ignored. This is the rule MCP server hints already follow.

### Worker output is hostile input

Results are capped and must be valid UTF-8, and a malformed response is an error result.
`PostToolUse` hooks run on the server, before the result is stored.

### Hooks get untrusted input

Hooks still run on the host, as you. Their input now comes from a sandbox the agent controls, so a
hook that runs its input, for example through `eval`, is a host escape. [Hooks](hooks.md) says so.

### Worker API

`durable.worker.v1.ToolService`, keyed by call ID so every call is idempotent:

| RPC | Purpose |
|---|---|
| `Info` | Hostname, user, working directory and environment, without the worker's token |
| `ListTools` | What the worker serves, for the server to check |
| `Execute(call_id, name, input, stream_output)` | Server stream: output deltas, then the result. A known call ID returns its result without running again |
| `Result(call_id)` | The result of a finished call, for recovery |
| `Cancel(call_id)` | Stops a running call |

- **A dropped connection doesn't stop a call.** The worker runs it detached, and the server recovers
  the result with `Result`. User cancel and lease loss send `Cancel`.
- **`stream_output` is false when a `PostToolUse` hook matches.** The server decides before the
  call, as `durable.WithoutToolOutput` does today, so live output never bypasses a hook.

### Results don't survive a worker restart

The worker keeps results in memory. After a restart, `Result` finds nothing, and recovery follows the
existing rules: a `ReplaySafe` call runs again, and a `ReplayNever` call records an unknown outcome.
Persisting results would add a store the agent can write to, for a case the harness already handles.

### One sandbox per session

- **Create** with the session, from a pinned image.
- **Stop** when the server closes an idle session, which parks the sandbox through approval waits
  and keeps its workspace. **Start** when the session opens again.
- **Connect** when the session opens and after each `sandbox start`, and reconnect when a call finds
  the connection gone. With a port forward, the port is a free loopback port chosen at open.
- **Pin** the sandbox name and image digest in the session directory, next to `provider.json`.

### Instructions and skills come from the workspace

The project `AGENTS.md` and project skills live in the workspace, so the worker reads them once when
the session is created, and the server pins them as it does today. `~/.durable/AGENTS.md` and user
skills stay on the server. Reading through the worker keeps the existing rules: symlinks that leave
the repository are refused, and the content is untrusted.

### MCP servers run in the sandbox

stdio MCP servers start inside the sandbox, beside the worker, and their credentials come from
OpenShell providers. The tool list stays pinned per session. This follows the second spike.

## The second spike

These need evidence before the slices are planned. The spike extends `spike/openshell-worker`.

1. **The workspace end to end.** `git clone` inside the sandbox, with egress allowed to GitHub only
   and the token from an OpenShell GitHub provider, so the agent sees a placeholder. Edits come back
   through `git push`. This also decides what the UI's directory picker becomes: a repo, not a path.
2. **Tools as a different user.** The agent has no Linux capabilities, so the worker can't switch
   user itself. Find out whether OpenShell or the image can run `bash` as another user, so the agent
   can't read the worker's environment. If not, record that the token isn't secret from the agent.
3. **A gateway restart.** What happens to running sandboxes, forwards and workers.
4. **End to end in the server.** `durable-server` with a spike flag that uses the remote tools,
   driven from the browser against a real model: create a session, approve a `bash` call and watch
   its output stream from the sandbox.
5. **Streaming and cancel.** `Execute` as a server stream with output deltas, and `Cancel` stopping
   a running `bash`, both over the launcher's connection.
6. **The SDK launcher.** Read `ai-openshell/sdk-go` and confirm what it covers: create, delete,
   upload, status, exec with streaming stdin, and port forwarding. Build the launcher on it, with a
   port forward if the SDK has one and the exec relay otherwise. Compare it with the first spike's
   CLI forward on latency, stdin limits (the CLI documents 4 MiB for piped input), recovery after a
   dropped connection, and the dependencies it adds. Record anything that still needs the CLI.

## Open questions

- **Before the slices:** how does a session name its workspace: a repo URL and branch, or an upload
  of a local directory?
- **Before the slices:** who builds the worker image, and how does a session choose one: a single
  image, or one per repository with its toolchain?
- **Later:** one sandbox per session, or one per repository shared across sessions?
- **Later:** a per-session egress policy, and how a person approves a new rule.
