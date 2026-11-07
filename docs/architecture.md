# Server architecture

**Status: phase 2 in progress.** Durable approvals, phase 1 and phase 2 slices A to F, S and R are
built, and the browser spike passed. G, H and I remain.

durable is a harness and a CLI today. This document sets the direction for running it as a service,
modeled on a CI system or distributed worker: the agent's commands run in a sandbox, and the UI and
supporting services run outside it.

## Goals

- Watch and steer a session from a browser, and close the browser without stopping the work. A long
  model outage is the exception: it [pauses](#server-session-lifecycle) the work until the session is
  next opened.
- Approve tool calls later, from a browser, without holding a process open.
- Run untrusted commands away from credentials and the session log.
- Keep every existing guarantee: crash-safe runs, exactly-once input and no double side effects.

Not goals for now: multi-tenant hosting, team features and an ACP or editor integration.

## Assumptions

- **Single user and self-hosted.** One SQLite file per session stays. The schema carries no identity,
  and `by` on an approval is free text. A team product would add auth, attribution and probably shared
  storage, and identity in the schema.
- **Local first.** Phases 2 and 3 run on loopback, with the browser and server on one machine. No
  proxy, no TLS and no remote access until [hosting](#hosting-is-deferred).

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
- **Epoch:** a cursor is only valid within one copy of the session, so `WatchEvents` takes and returns
  `(epoch, after_seq)`. A restored copy reuses seqs from an older point, and without an epoch a client
  silently skips the new events up to its old cursor. See below.
- **Notify, then re-read:** the server's `Observer` only signals that the seq advanced, and each stream
  reads `Events(cursor, limit)` itself. Catch-up and live tail share one path, so the handoff can't
  drop or repeat an event, and a slow client only delays its own stream.
- **Heartbeats:** `WatchEvents` sends one every 15–30s, because approval waits can last hours and
  proxies close idle streams.
- **Deltas carry the last committed seq,** so a client drops any delta older than the events it has.
- **Deltas are opt-in** with `include_deltas`, and a delta is sent after every event up to its
  `last_seq` and before any later one. Each watcher has a bounded buffer and never slows a run. On
  overflow, text is skipped until the next reset, so a slow client never shows garbled prose, while
  tool output loses only the dropped chunks.
- **Headers go out at once,** after the delta subscription, so a client's `WatchEvents` call returns
  before the first event, and a submit made after it returns is seen from its reset.
- **Storage is unchanged:** SQLite stays JSON, because `Raw` must replay byte for byte. Tool input is
  `bytes` in proto, not `google.protobuf.Struct`. `Raw` stays off the wire, because provider content
  is only replayed server side.
- **API types don't mirror `Event`:** the conversion layer redacts `Event.Call`, which can hold
  secrets a gate added.

**Cost:** `buf` and generated code, a public schema that needs `buf breaking` once there are external
clients, and care with [stream timeouts and end-of-body errors](https://connectrpc.com/docs/go/streaming/).

Connect doesn't give ACP compatibility. [ACP](https://agentclientprotocol.com/overview/architecture)
is JSON-RPC and would be a separate adapter.

### The event log is the change feed, with an epoch

The `events` table is already a logical WAL: append-only, ordered by `seq`, and committed with each
state change. Clients follow it, not a physical WAL as in Litestream, because page-level replication
would expose the whole file, including `Event.Call` and `Raw`, and can lag committed work.

Litestream's one useful idea here is the generation, which we call the epoch:

- A one-row `session_epoch` table holds a random ID. It's a new table, so `migrate` adds it to
  existing sessions, and the first open fills it in.
- A restore or `VACUUM INTO` copy writes a new epoch. A server restart keeps the old one, so clients
  resume rather than replay.
- When the client's epoch doesn't match, or its `after_seq` is past the newest seq, the server returns
  `FAILED_PRECONDITION` with the current epoch. The client discards its state and replays from 0.
  A cursor without an epoch (`after_seq > 0` and an empty epoch) is treated as a mismatch.
- The seq check also catches most copies restored by hand, which keep the old epoch. Restores should go
  through `durable.Restore`.

### Snapshot and restore

The library gets two functions, with no CLI command until there's a need for one:

- `Harness.Snapshot(ctx, dst)` runs `VACUUM INTO dst`. It's a read, so it works while a run holds the
  lease, and it gives a consistent copy without stopping the session.
- `durable.Restore(ctx, src, dst)` copies `src` to `dst` and writes a new epoch into the copy. It
  refuses when `dst` has a live lease, and removes a stale `-wal` and `-shm` before the copy goes live.

The epoch is written at restore, not at snapshot. If it were written at snapshot, restoring the same
snapshot twice would give two histories with one epoch, and a client would skip events.

### Session index

A session's path is a one-way hash of its working directory and name, so the server can't name a
session from its path alone. Each session directory gets a `session.json` next to `provider.json`,
holding the working directory, the session name and the creation time.

- Whoever creates the session writes it, the CLI or the server, once and atomically.
- The CLI writes it for existing sessions the next time it opens one.
- The server lists sessions by reading `~/.durable/sessions/*/session.json`, and skips directories
  without one.

A file per session follows the `provider.json` pattern and avoids a shared database that every
session would write to.

### Server session lifecycle

The server opens a session's `Harness` on demand, when an RPC names it, and closes it once it's idle
for a few minutes, meaning no run and no watchers. That bounds open files and SQLite connections.

The server holds the lease while a session is open, so the CLI waits on the same session as it would
for another CLI. To hand a session back to the CLI, close the browser tab and wait out the idle
timeout.

- **Outages pause unattended work.** A failed run retries with backoff. With no clients, an outage
  keeps retrying for one idle timeout after it starts, then the session closes and releases the lease.
  The work stays paused until a client or the CLI next opens the session, even if the provider
  recovers sooner, and `ListSessions` doesn't show it as paused. A server restart pauses work the
  same way, because sessions only open on demand. Reopening sessions with unfinished work on a timer
  would remove the pause, and waits until it's needed.
- **Lost lease.** If another process takes the lease, the run driver stops, RPCs and watch streams
  return `UNAVAILABLE`, and the registry drops the session so the next request opens it fresh.

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

### Hosting is deferred

Reaching the server from another machine needs a proxy and TLS in front of it, and that waits until
there's a hosting target. It brings:

- **Proxy streaming.** Buffering proxies can hold a stream until it ends. Turn buffering off first,
  for nginx `proxy_buffering off` with `proxy_http_version 1.1`, and test with nginx in
  `e2e/compose.yaml`. Heartbeats already cover idle timeouts.
- **Allowed hosts and origins.** The server accepts only its listen address, `localhost` and
  `127.0.0.1` on its port, and `http` origins. A proxied server needs configurable hosts and origins,
  including `https`.
- **TLS** at the proxy.
- **SSE fallback.** If a proxy defeats streaming over `fetch`, serve `WatchEvents` as SSE next to
  Connect, for example with [connect-sse](https://pkg.go.dev/github.com/firetiger-oss/connect-sse),
  rather than changing the schema.

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

## Validation

These tests prove the streaming model before the UI depends on it. Layers 1 to 3 are phase 2's
acceptance tests, and layer 4 opens phase 3.

### 1. Real-binary tests

These live in `cmd/durable-server`. They share one binary built by `TestMain`, use a temporary `HOME`,
and drive a scripted fake model.

- **Contiguity:** every client sees strictly increasing seqs with no gaps, and together they match
  `Events(0)`.
- **Crash and resume:** `kill -9` the server mid-stream, restart it, and resume from `after_seq` with
  no gaps or duplicates. The epoch must be unchanged.
- **Restore:** restore an older copy, then append. A client holding the old cursor gets
  `FAILED_PRECONDITION`, replays from 0 and sees the new events.
- **Approvals:** one client decides while another watches. A duplicate decision is idempotent, and
  the call runs once.
- **Protocols:** run the suite over Connect, gRPC and gRPC-Web with the connect-go clients.
- **Redaction:** secrets a gate adds to `Event.Call` never reach the wire.
- **Auth:** requests with a missing token, a wrong `Origin` or a wrong `Host` are rejected.

### 2. Contention

Run one scripted session with 0, 10 and 50 watchers, one of which never reads. `Run`'s wall time stays
within a bound, and the stalled watcher doesn't delay the others. This checks the single-connection
pool under watcher reads.

### 3. Network faults

A fault-injecting TCP proxy inside the real-binary tests (`cmd/durable-server/faults_test.go`) adds
latency, a TCP reset mid-stream, a closed connection mid-stream and a break during a heartbeat wait.
The client resumes from its last seq and ends with the same contiguous seqs as a clean watch. The
breaks run over HTTP/2 for Connect, gRPC and gRPC-Web, and over HTTP/1.1, the browser's transport,
for Connect and gRPC-Web. Every break surfaced as an error, never a clean end. It replaces toxiproxy,
because it needs no Docker or new dependency and runs in plain `go test`.

Not covered: a response that ends cleanly without Connect's end-of-stream message, which an
HTTP-aware proxy can produce on a timeout. It needs a real proxy, so it waits for
[hosting](#hosting-is-deferred) with the nginx tests.

### 4. Browser

A minimal test page uses connect-web's `WatchEvents` on loopback. It checks that streaming over
`fetch` works on HTTP/1.1, then restarts the server and checks that the page resumes. If this fails,
fall back to SSE for the browser. Phase 3 automates it with Playwright, run from its Docker image.

**Spike result: passed.** The page lives on the unmerged
[`spike/browser-watch`](https://github.com/wolfeidau/durable/tree/spike/browser-watch) branch. It was
driven by hand in Chrome against a real OpenRouter model:

- `WatchEvents` was one `fetch` over HTTP/1.1, open for 50s, and events arrived as they committed,
  not when the stream ended. Heartbeats arrived every 5s while idle.
- An approval parked, Allow from the page ran the call once, and two tabs showed identical seqs.
- After `kill -9` and a restart, the page resumed from its cursor under the same epoch with no gaps
  or duplicates.
- After `durable.Restore`, the page got `FAILED_PRECONDITION` with a decodable `EpochMismatch`
  detail, replayed from 0 and then followed new events.

What phase 3 needs from it:

- **Stable auth across restarts.** The token is new on each run, so after a restart the page stops
  at `Unauthenticated` until it's given the new one. Keep the token across restarts, or move the
  browser to a cookie set from a one-time link.
- **The hook's shape.** It holds `(epoch, cursor)`, counts a seq that isn't `cursor + 1` as a gap,
  resets to `("", 0)` on `FAILED_PRECONDITION`, waits for new auth on `Unauthenticated`, and
  otherwise reconnects after a 1s backoff.
- **Toolchain.** `protoc-gen-es` has a `node` shebang, so with Bun alone run it through
  `bun run --bun`.

### 5. Remote workers (phase 4)

A worker takes assignments over a server stream and reports through an idempotent unary
`Complete(call_id, result)`. If the worker crashes after running a call but before `Complete`, the call
takes the `ReplayNever` unknown-outcome path and doesn't run again.

## Phases

1. **Live deltas.** Harness only.
2. **Server and API.** `cmd/durable-server`, `api/`, the five RPCs (`Submit`, `Cancel`,
   `DecideApproval`, `WatchEvents`, `ListSessions`), the session index, token auth, the session epoch, and
   validation layers 1 to 3. In phase 2 the server serves only sessions `durable chat` created: it opens
   existing sessions and never creates one. Each slice is one PR:

   | Wave | Slice | Depends on | Status |
   |---|---|---|---|
   | 1 | **A.** Session epoch, `Snapshot` and `Restore` | | #50 |
   | 1 | **B.** Session index | | #51 |
   | 1 | **C.** API schema, `buf` toolchain and the redacting conversion layer | | #52 |
   | 2 | **D.** `cmd/durable-server` skeleton, token auth and the binary test harness | C | #53 |
   | 2 | **S.** Session assembly extracted from `chat` | | #54 |
   | 3 | **R.** Session registry | D, S | #56 |
   | 3 | **E.** `WatchEvents`, with layer 1 stream tests | A, D, R | #58 |
   | 3 | **F.** `Submit`, `Cancel`, `DecideApproval` and `ListSessions`, with layer 1 approval tests | B, D, R | #57 |
   | 4 | **Spike.** A throwaway page streams `WatchEvents` on loopback (layer 4) | E | [Passed](#4-browser) |
   | 5 | **G.** Deltas on the stream | E | #66 |
   | 5 | **H.** Layer 2 contention tests | E | Next |
   | 5 | **I.** Layer 3 network fault tests | E | #64 |

   The spike comes before G to I because, if it fails, falling back to SSE changes C and E.

   Known gaps in the built slices:

   - **Open resumes work.** Opening a session kicks its run driver, so watching a session resumes its
     queued work. That includes a copy made by `durable.Restore`, which conflicts with "a restored
     session must not auto-resume" below.
   - **Approval status on request.** An `approval_requested` event carries no status, so it reaches
     the wire as `APPROVAL_STATUS_UNSPECIFIED`. Clients treat the kind as pending. Setting
     `APPROVAL_STATUS_PENDING` would be clearer.
3. **Web UI.** One session view with a live transcript and an approval prompt. Prove the streaming hook
   first, with validation layer 4.
4. **Remote execution.** An outbound worker, a remote `Tool`, Docker, and workspace snapshots.
5. **Scale.** Queues, multiple workers and microVMs.

## Open questions

- **Before slice G:** should a restored session wait for an explicit resume before the server runs it?
- **Before hosting:** where does it run, and which proxy sits in front? That decides the proxy tests
  and whether the SSE fallback is needed.
- **Before phase 4:** are containers acceptable? [Issue #9](https://github.com/wolfeidau/durable/issues/9)
  rejected them for local use because the agent needs your real toolchain. Remote workers reverse that
  with per-repo images. Native OS sandboxing stays deferred.
- **Before phase 4:** where do `AGENTS.md` and skills load from, given they live in the remote workspace?
- **Before phase 4:** how do git credentials reach the sandbox without the agent holding them?
- **Before phase 5:** Firecracker or [gVisor](https://fly.io/learn/firecracker-vs-gvisor/)?
