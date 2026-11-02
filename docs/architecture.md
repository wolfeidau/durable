# Server architecture

**Status: proposed, for review before phase 2.** Nothing here is built yet except durable approvals.

durable is a harness and a CLI today. This document sets the direction for running it as a service,
modeled on a CI system or distributed worker: the agent's commands run in a sandbox, and the UI and
supporting services run outside it.

## Goals

- Watch and steer a session from a browser, and close the browser without stopping the work.
- Approve tool calls later, from anywhere, without holding a process open.
- Run untrusted commands away from credentials and the session log.
- Keep every existing guarantee: crash-safe runs, exactly-once input and no double side effects.

Not goals for now: multi-tenant hosting, team features and an ACP or editor integration.

## Assumptions

- **Single user and self-hosted.** One SQLite file per session stays. A team product would add auth,
  attribution and probably shared storage, so confirm this before phase 2.

## Architecture

```text
 Browser / TUI                 Control plane                       Sandbox
+--------------+   Connect   +-----------------------+  outbound  +------------------+
| Web UI       | ----------> | durable-server        | <--------- | Worker           |
| (unary and   | <---------- |  - owns the harness   |  work and  |  - runs bash and |
|  stream)     |   events    |  - session.sqlite     |  results   |    file tools    |
+--------------+   deltas    |  - model key, hooks,  |            |  - workspace     |
                             |    MCP servers        |            |  - no credentials|
                             +-----------------------+            +------------------+
                                        |
                                        v
                                  Model provider
```

The server process holds the session lease, so it's the only path to a live session. Clients never
open the SQLite file. The sandbox holds the workspace and nothing else.

## Decisions

### The harness runs outside the sandbox

The sandbox is a remote `Tool` executor, as in [Claude Managed Agents](https://www.anthropic.com/engineering/managed-agents).
The alternative is running the loop inside, as [Ramp's Inspect](https://builders.ramp.com/post/why-we-built-our-background-agent) does.

| | Inside the sandbox | Outside (chosen) |
|---|---|---|
| Code change | Small | Remote `Tool` and a worker |
| Model key | In the sandbox unless proxied | Out of reach |
| `session.sqlite` | Reachable by `bash`, so the log can be altered | Out of reach |
| Sandbox lost | Session lost | Session survives |

`Tool.Execute` already has the shape of `execute(name, input)`, and a lost sandbox maps to the
existing `ReplayNever` unknown-outcome path.

**Cost:** the session can outlive its workspace. After a sandbox loss the transcript describes edits
that no longer exist, so remote execution needs snapshots or frequent branch pushes. Each tool call
also crosses a network.

### Workers dial out

A worker connects out to the server and asks for work, following
[Buildkite's agent model](https://buildkite.com/docs/agent/v3). No inbound ports are needed, so a
worker can run anywhere, including on your own machine.

A session differs from a CI job: it's long-lived and waits on people. The sandbox should be released
or parked during those waits.

### Connect for the API

[Connect](https://connectrpc.com/docs/introduction/) gives one schema for the browser and the worker,
and one handler serves Connect, gRPC and gRPC-Web.

- **Shape:** unary commands plus one server stream. No bidi, because [browsers can't do it](https://connectrpc.com/docs/web/getting-started/).
- **Resume:** `WatchEvents(after_seq)` uses `Event.Seq` as the cursor. The client owns reconnecting.
- **Storage is unchanged:** SQLite stays JSON, because `Raw` must replay byte for byte. Use `bytes` for
  `Raw` and tool input in proto, not `google.protobuf.Struct`.
- **API types don't mirror `Event`:** the conversion layer redacts `Event.Call`, which can hold
  secrets a gate added.

**Cost:** `buf` and generated code, a public schema that needs `buf breaking` once there are external
clients, and care with [stream timeouts and end-of-body errors](https://connectrpc.com/docs/go/streaming/).

Connect doesn't give ACP compatibility. [ACP](https://agentclientprotocol.com/overview/architecture)
is JSON-RPC and would be a separate adapter.

### The web UI is the primary client

Background work, delayed approvals and watching several sessions suit a browser. The CLI stays as it
is, and a TUI can be another client later.

The stack is Bun, React, TanStack Router and Query, and `connect-query`, embedded in the server
binary. `connect-query` suits unary calls; `WatchEvents` needs a hand-written hook.

### Separate server binary

`cmd/durable-server` keeps Connect and the embedded UI out of `cmd/durable`. The embed directory has a
committed placeholder and a "UI not built" page, so `go build`, `go vet ./...` and `go test ./...`
work on a fresh clone without Bun. `buf` is pinned through mise, so the library's `go.mod` stays small.

### Durable approvals

Shipped in v0.1.0. `Options.DeferApprovals` parks a call as a row and an event, and `Run` returns
`ErrAwaitingApproval`. An approved call runs its stored input and is never re-gated, so hooks run once
and the input can't change after approval.

### Live deltas are not durable

Committed events are the source of truth. Deltas are a best-effort view of work in progress, and a
client rebuilds from events after reconnecting.

- `Options.OnDelta` receives model text and `bash` output. It must not block, so a server buffers and
  drops on its side.
- Deltas travel through `ctx`, around `Model.Generate` and `Tool.Execute` only. That leaves `Model` and
  `Tool` unchanged, gives wrappers nothing to forward, and keeps compaction silent.
- A reset delta before every model attempt tells clients to discard partial text.
- Tool output must be valid UTF-8 and is capped per call.
- **Live tool output is suppressed when a `PostToolUse` hook matches**, because those hooks can redact
  or withhold output and a delta would bypass them.

### Litestream is deferred

[Litestream replicates asynchronously](https://litestream.io/tips/), so a restore can lose committed
work. If a `started` commit is lost after `git push` ran, the harness sees no started call and may
repeat the push.

It may come later as an off-host backup sidecar, never as failover. A restored session must not
auto-resume, and it doesn't protect the workspace. For export, use `Harness.Events` for audit data, or
`VACUUM INTO` on an idle session for a full copy.

### CGO stays for now

Releases are source only. Binaries wait for phase 3, when the Bun build needs a release pipeline
anyway. Moving to `modernc.org/sqlite` would change the driver under the durability guarantees, so it
needs its own `kill -9`, lease and power-loss evaluation.

## Security

The API can approve shell commands, so the bar is higher than for a typical dashboard.

- Require a per-run token on every RPC, even on loopback, because any web page can send requests to
  localhost.
- Validate `Host` and `Origin`, and leave CORS off.
- Never mark `DecideApproval` as side-effect free, so it can't be called through GET.
- Treat exports, replicas and event streams as sensitive.

`by` on an approval is attribution, not authentication.

## Phases

1. **Live deltas.** Harness only.
2. **Server and API.** `cmd/durable-server`, `api/`, the four RPCs (`Submit`, `Cancel`,
   `DecideApproval`, `WatchEvents`), a session index and token auth. Session paths are one-way hashes,
   so the index is what lets the server name a session. A real-binary test kills the server mid-stream
   and resumes from `after_seq`.
3. **Web UI.** One session view with a live transcript and an approval prompt. Prove the streaming hook
   first.
4. **Remote execution.** An outbound worker, a remote `Tool`, Docker, and workspace snapshots.
5. **Scale.** Queues, multiple workers and microVMs.

## Open questions

- **Before phase 2:** is single-user and self-hosted the right scope?
- **Before phase 4:** are containers acceptable? [Issue #9](https://github.com/wolfeidau/durable/issues/9)
  rejected them for local use because the agent needs your real toolchain. Remote workers reverse that
  with per-repo images. Native OS sandboxing stays deferred.
- **Before phase 4:** where do `AGENTS.md` and skills load from, given they live in the remote workspace?
- **Before phase 4:** how do git credentials reach the sandbox without the agent holding them?
- **Before phase 5:** Firecracker or [gVisor](https://fly.io/learn/firecracker-vs-gvisor/)?
