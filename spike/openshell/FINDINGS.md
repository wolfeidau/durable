# Spike findings: a tool worker in an OpenShell sandbox

The server connects in to a worker inside an OpenShell sandbox through a port forward, and runs the
five tools through `durable.Tool`. `go run ./spike/openshell` passes all nine steps against a local
gateway (OpenShell 0.1.7, Docker Desktop, macOS arm64) and cleans up after itself.

## Results

| Step | Result |
|---|---|
| Build, create, connect | Docker build 2 s warm (17 s after a code change, about 35 s cold). Create 0.5 to 0.8 s. The worker answered its first `Info` 2 to 3 ms after create returned |
| Env vars | `HOME=/sandbox`, `USER=1500`, `SHELL=/bin/bash`, `TERM=dumb`, `OPENSHELL_SANDBOX=1`, `PATH`, and CA variables pointing at the supervisor's TLS CA. No credentials or proxy variables with `--no-auto-providers` |
| Upload | 3 files in about 85 ms, owned by the sandbox user |
| Tools | All five work through the adapter: `list_files`, `read_file`, `grep`, `edit_file` (confirmed by a re-read), `bash` |
| Egress | `curl https://example.com` fails in 2 ms (exit 7). Default deny holds |
| Latency | 20 × `bash true`: p50 about 1 ms, max 3.6 to 4.5 ms |
| Recovery | After `forward stop`, an in-flight `Execute` fails with unexpected EOF, but the worker finishes the call. After `forward start`, `Recover` returns `done`, and a repeat `Execute` with the same call ID returns the stored result without re-running |
| Untrusted metadata | A worker that claims `bash` is read-only and adds `exfiltrate` is ignored: `bash` still needs approval, and `exfiltrate` is dropped |

## Connect over h2c through the forward

It works with no HTTP/1.1 fallback. The worker serves unencrypted HTTP/2 with Go's
`http.Protocols.SetUnencryptedHTTP2`, and the client uses the same setting. The forward is an
`ssh -L` child of the CLI, so it's a byte pipe. Plain HTTP/1.1 Connect also works.

## Bind address

`--forward <port>` with no bind address listens on `127.0.0.1` only on the host. The worker listens
on `127.0.0.1` inside the sandbox, which the forward reaches, so it isn't reachable from other
containers.

## Image requirements

- The image follows OpenShell's bring-your-own-container example: a non-root `app` user (uid and
  gid 1500), `/sandbox` owned by it as `WORKDIR`, and `USER app`.
- The supervisor starts as root, drops children to that user, and replaces `CMD` with the
  `-- <cmd>` given to `create`.
- A locally built tag works with `--from`. No networking tools are needed in the image.
- Sandbox names are limited to 19 characters.
- The background forward inherits the CLI's pipes, so piping `create` into another command hangs.
  The launcher writes CLI output to a file instead.

## Upload

The default destination is the working directory (`/sandbox`), and a directory keeps its base name.
An explicit destination becomes the parent. Inside a git work tree, `.gitignore` is applied by
default; `--no-git-ignore` turns it off.

## Sandbox stop and start

- `sandbox stop` removes the forward, and the port refuses connections.
- `sandbox start` restarts the worker with its original command and environment, and keeps the
  workspace. It does **not** restore the forward, and the worker's in-memory results are lost.
- Deleting a sandbox stops its forward.

Gateway restarts weren't tested.

## Security

- **The worker's token is readable by the agent.** It's in the worker's environment, and `bash`
  runs as the same uid, so `/proc/$PPID/environ` shows it. The impact is small: the agent can
  already run the tools, and the sandbox has no route to the server. The token keeps other host
  processes off the forwarded port; it isn't a secret from the agent.
- `--env-from` prints a warning that the agent can read the value, which matches the above.
- On the host, `ps` shows the forward's `ssh` command line with an OpenShell session token.
- Worker metadata is untrusted. The host takes `read_only` and `replay` from its own table, and
  only for tool names it knows.

## Fallback

Nothing forced the fallback, where the server starts the worker with `openshell sandbox exec` and
talks over stdio with yamux. It wouldn't fix the two gaps above either: the forward needs
supervising, and results are lost when the worker restarts.

## Open questions for the phase 4 spec

1. Persist worker results across worker restarts, or accept that a restart turns an in-flight call
   into an unknown outcome?
2. Who supervises the forward, including after `sandbox start`?
3. Run tools as a different uid from the worker, so the agent can't read the worker's environment?
4. How does the workspace arrive (upload, volume or git clone, which needs an egress rule), and how
   do edits come back?
5. A per-session egress policy file.
6. Who builds and pins the image, and how are tool schema mismatches between server and worker
   caught?
7. `Execute` is unary. Live `bash` output needs a streaming RPC, and it must still respect the
   `PostToolUse` hook rule.
8. A dropped request deliberately doesn't stop the worker's call, so user cancel and lease loss
   need a `Cancel(call_id)` RPC.
9. One sandbox per session means allocating a port per forward.
10. Connect sends a request's deadline to the worker, which can answer `canceled` before the
    client's own context ends, so the adapter records a tool error instead of stopping. The harness
    cancels rather than setting deadlines, but the adapter should treat a `canceled` or
    `deadline_exceeded` code as cancellation only when its own context has ended, and the worker
    API should say whether deadlines are used at all.

## Spike 2: SDK launcher

`go run ./spike/openshell -launcher=sdk` (default) and `-launcher=cli` both pass every step on the local
gateway (OpenShell 0.1.7) and leave `sandbox list` and `forward list` empty. The SDK is
`github.com/ai-openshell/sdk-go` v0.1.7, imported only from `internal/remote` and the spike runner.

### mTLS client workaround

`gateway.NewClient("")` returns `ErrUnsupportedAuthMode` for `auth_mode: "mtls"` only when no auth is
supplied. This works: `gateway.LoadConfig("")` for the gateway name and directory, then
`gateway.NewClient(cfg.Name, gateway.WithAuth(v1.NoAuth()), gateway.WithTLS(&types.TLSConfig{CertFile, KeyFile, CAFile}))`
pointing at `<dir>/mtls/{tls.crt,tls.key,ca.crt}`. No fallback to `v1.NewClient` was needed. The workspace
is `default` (confirmed through `Workspaces().ListAll`).

### Forward as a dial

`TCP().Forward(ctx, ws, sandbox, port)` returns an `io.ReadWriteCloser` that wraps into a `net.Conn` (no
deadlines) and serves as `http.Transport.DialContext`. HTTP/2 cleartext works over it, with no HTTP/1.1
fallback. Two details: the stream's context must be detached from the dial context (the request's context
ends the stream otherwise), and each connection is one gRPC stream through the gateway, so there's no local
port, no `ssh` child and no forward to supervise. A lost connection is replaced by the transport's next dial.

### Upload

`Files()` is unusable, so upload is `Exec().Interactive` running `tar -x`. Findings:

- Interactive allocates a PTY. `tar` refuses a terminal on stdin even with `-f -`, and cooked mode mangles
  binary data. The command is `stty raw -echo && echo READY && head -c N | tar -xf - -C dest`; the client
  waits for `READY` before writing (data sent earlier was mangled and killed the shell).
- The PTY never signals EOF (`CloseInteractiveInput` is not enough), so the archive length is known up front
  and `head -c` reads exactly that many bytes. The archive is built in memory.
- The gateway rejects stdin messages over 1 MiB (`decoded message length too large`), and each `Write` is
  one message, so writes are chunked at 256 KiB.
- Result: a 10 MiB random file arrives with matching size and sha256 (verified by the `bash` tool inside the
  sandbox), in about 0.1 s for the transfer. The CLI's `--no-git-ignore`/gitignore behaviour has no
  equivalent; the tar copies everything, including `.git` and ignored files.

### Streaming and cancel

- `for i in 1..5; do echo $i; sleep 0.3; done`: deltas arrive at +1 ms, +309, +616, +922 and +1.23 s, and the
  result at +1.54 s, so output is live, not batched. The CLI forward gives the same timing.
- Cancel: `sleep 30; echo late`, context cancelled at 1 s. `Execute` returns `context.Canceled` 5 ms later.
  The worker stores the result `context canceled` (it appears about 2 s after, while the bash tool's process
  group shuts down), which doesn't contain `late`; a repeat `Execute` with the same id returns that stored
  result without re-running.

### Connection loss, stop and start

- Closing every forward stream mid-call fails the in-flight `Execute` (`unavailable: Cancelled`). The worker
  finishes the call, and `Recover` on a fresh dial returns `done`, as with the CLI forward. The adapter
  didn't cancel the call because its own context was still live.
- `Stop` + `WaitStopped` took about 0.16 s (CLI 0.4 s); `Info` while stopped returns `Conflict: sandbox is
  not ready`. `Start` + `WaitReady` took about 1.5 s (CLI 0.6 s). The next call needed only a new dial: no
  forward to restart. The workspace survives; worker results don't.

### Latency (50 x bash `true`)

| Launcher | min | p50 | p95 | max |
|---|---|---|---|---|
| sdk | 0.81 ms | 1.29 ms | 1.84 ms | 2.05 ms |
| cli | 0.72 ms | 0.95 ms | 1.34 ms | 4.16 ms |

A second run gave sdk p50 1.50 ms / p95 1.88 ms / max 3.42 ms and cli p50 1.38 ms / p95 4.07 ms / max 4.81 ms, so the two are within noise: both are about 1 to 1.5 ms per call.

### Dependencies

`go.mod` gains `github.com/ai-openshell/sdk-go v0.1.7` and 5 indirect modules (`cespare/xxhash/v2`,
`google.golang.org/grpc`, `genproto/googleapis/rpc`, `golang.org/x/net`, `golang.org/x/text`), and bumps
`go-logr/logr`, the `otel` modules, `auto/sdk`, `x/oauth2` and `x/sync`. `go mod graph` lists 105 distinct
modules in total. The `durable-worker` binary doesn't link the SDK or gRPC.

### API changes

- Worker API v2: `Execute` is a server stream of `delta` then one `result`, with `stream_output` in the
  request; `Cancel(call_id)` is new. Auth is an interceptor on both unary and streaming handlers (a unary-only
  interceptor would leave streaming open).
- Root package, needed by the real slices: `durable.WithToolOutputSink(ctx, fn)` and
  `durable.ToolOutputEnabled(ctx)` in `delta.go`. The adapter sets `stream_output` from the latter and the
  worker captures tool output with the former.
- The adapter treats `canceled`/`deadline_exceeded` as cancellation only when its own context has ended, and
  sends `Cancel` on a detached 3 s context at that point.

### Still needs the CLI

Nothing in the runner. The image build uses `docker`, and the final `sandbox list`/`forward list` check uses
the CLI only to confirm. Gitignore-aware upload is CLI-only.

### New open questions

1. The PTY-based upload needs the length up front and an in-memory archive. Is there a non-PTY exec path, or
   should the worker offer an `Upload` RPC instead?
2. Worker results are still lost on `Start`; `Cancel` after a restart is a no-op.
3. The cancel result takes about 2 s to be stored after the bash tool is cancelled; the harness must accept
   `Recover` returning "no result" for that window.
4. mTLS needs the `NoAuth` plus `WithTLS` workaround; confirm it's supported rather than accidental.
5. Gateway-relayed streams share the gateway's availability: a gateway restart drops every session's connection.
   Untested.
6. Deltas are dropped when the worker's 1024-entry buffer fills (best-effort by design).

## Spike 2b: end to end in durable-server

### Wiring points

- `serve --sandbox-image <ref>` (empty keeps today's behaviour). `main.go` builds an `SDKLauncher` and
  `remote.Sandboxes`, and passes `Sandboxes.Tools` as `RegistryConfig.Tools`.
- `commands.AssembleConfig.Tools func(ctx, sessionID) ([]durable.Tool, func(), error)`: used only when non-nil, so
  `durable chat` and the default path are untouched. `Assemble` still builds `tools.All` locally, checks the
  remote tools against it (name, order, description, schema, strict flag), fails the open on any difference,
  then overwrites the local five in place. MCP and skill tools are appended after, and hooks wrap the result as
  before. The returned func runs from `Assembled.Close`, so it stops the sandbox when the registry closes an idle
  session, on a failed open and on server shutdown. Nothing deletes a sandbox; `durable-server sandbox-delete
  <session id>` does.
- Sandbox name is `ds-<16 hex session id>` (19 chars, the OpenShell limit). `ensure` does Get, then NotFound ->
  Create+WaitReady, Stopped -> Start+WaitReady, Ready -> reuse. The worker starts with `--root /sandbox`.
- Calls dial through `SDKLauncher.Dial`, one forward stream per connection, so a stop/start needs no
  reconnect logic: each open of the session dials fresh.
- `Assemble` needed a `//nolint:gocognit`; split it in the real slice.

### Lifecycle and approval evidence

`TestSandboxRunsToolsInTheSandbox` passed in four runs against the local gateway before it stopped provisioning
(see below). Scripted model: `list_files` (no approval), then `bash` `echo hello-from-sandbox; sleep 0.5; id -u`.
- `list_files` committed a result with no approval. The bash call parked as exactly one pending approval and
  ran once after `DecideApproval(allow)`.
- The watch showed live tool-output deltas containing `hello-from-sandbox` before `call_done`, and the committed
  result contained `hello-from-sandbox` and `1500`, the sandbox user, so it did not run on the host.
- First run (Submit to done, including sandbox create and approval polling): about 0.63 s with the image cached.
  With `--idle-timeout 3s` the sandbox reached Stopped about 4.0 s after the run. Reopen (Start + WaitReady +
  dial + second bash, with approval polling): about 0.63 s.

### What broke

- After those passing runs the gateway stopped provisioning: new `ds-*` sandboxes sat in Provisioning, then went
  to Error ("Sandbox provisioning deadline expired" in the gateway log). The last runs of the test FAILED at
  Submit with `internal: internal error`, so the final verification run did not pass. I did not restart the
  gateway and did not find the root cause. The same code passed four times, so I suspect the environment, but
  that is unproven.
- A provisioning failure reaches the API client as an opaque `internal: internal error`.
- The test needs `XDG_CONFIG_HOME` pointed at the real config dir, because the SDK finds the gateway config
  under `$HOME` and tests use a temporary HOME.
- The launcher's `Close` must be a `t.Cleanup` registered first, not a `defer`, or the delete in the cleanup runs
  on a closed client and leaks the sandbox (this happened once).
- A watch from seq 0 replays the earlier `done`, so the test waits for the second one.

### Open questions for the real slices

1. Worker token: kept in server memory, so after a server restart an existing sandbox can't be reached. The
   server refuses and points to `sandbox-delete`, which loses the workspace. Persist the token in the session
   dir (0600) or rotate it when the sandbox starts.
2. Pinning: the name derives from the session ID, but the image ref should be pinned per session (like
   `provider.json`) so a reopen can't change it.
3. AGENTS.md and skills still load from the host; the sandbox workspace never sees host files. They need to come
   through the worker, or an upload.
4. Gateway down or slow: `Assemble` blocks for the create plus up to 30 s of worker-ready retries, and the API
   returns an opaque internal error. The UI needs a typed `unavailable` error with the cause.
5. Stop on close uses a detached 60 s context, so shutdown can wait that long per session; stop in parallel.
6. Two sessions opening at once serialise on the `Sandboxes` mutex for the whole create; narrow the lock.
