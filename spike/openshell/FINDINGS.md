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

## Spike 2a: workspace through git

`go run ./spike/openshell/workspace` passes end to end, and `-users` runs the tool-user probes. Both clean up
their sandboxes, and the first also removes the provider and profile. An earlier attempt was blocked by a wedged
container runtime.

### Observed

- **Provider mechanism.** The SDK creates the provider from a value: `Providers().Create` with
  `Spec.Credentials{"GITHUB_TOKEN": <value>}`. The CLI equivalent is `--credential GITHUB_TOKEN` (reads the env
  var) or `--from-existing`. The runner reads `gh auth token` into memory and sends it only to the gateway.
- **Profile needs the CLI.** The gateway has no built-in profiles; a provider's `--type` must name an imported
  profile. The SDK's `ProviderProfile` endpoints have only host, port and protocol, so they can't carry L7 rules,
  and the gateway rejects both an L7 endpoint without rules and an L4 credentialed endpoint
  (`credentialed endpoint 'github.com:443' uses L4-only`). The runner therefore imports a profile file with
  `openshell profile import -f` (workspace scope), with `github.com:443` rules limited to the scratch repo's
  `info/refs`, `git-upload-pack` and `git-receive-pack`, and binaries `/usr/bin/git` and
  `/usr/lib/git-core/git-remote-https`.
- **Attach and policy at create.** `SandboxSpec.Providers` and `SandboxSpec.Policy` work. The runner also passes
  the tutorial's repository-scoped `network_policies` entry. The agent's environment held only
  `GITHUB_TOKEN=openshell:resolve:env:<handle>_GITHUB_TOKEN`; `GH_TOKEN` was not set. Proxy-related CA
  variables (`GIT_SSL_CAINFO`, `SSL_CERT_FILE`) point at the supervisor CA, and github.com resolves to a
  proxy-local address with a certificate issued by "OpenShell Sandbox CA".
- **Policy decisions.** `git clone` was allowed at L4 and L7
  (`ALLOWED GET https://github.com/<repo>.git/info/refs [policy:_provider_<name> engine:l7]`).
- **Auth form.** Sending `Authorization: Bearer $GITHUB_TOKEN` through `http.extraHeader` got a `401` from GitHub
  (git smart HTTP wants Basic). The docs say the proxy decodes and re-encodes Basic auth, so the runner uses a
  credential helper with username `x-access-token` and password `$GITHUB_TOKEN`. That works for clone and push.
- **Cleanup ordering.** Sandbox deletion is asynchronous, so deleting the provider right after fails with
  `Conflict: provider ... is attached to sandbox(es)`, and the profile with `... in use by providers`. The runner
  retries both.

### Policy used

The profile (imported with the CLI) has the single endpoint `github.com:443`, protocol `rest`, enforce, with
allow rules `GET /wolfeidau/durable-sandbox-scratch.git/info/refs*`, `POST .../git-upload-pack` and
`POST .../git-receive-pack`, and binaries `/usr/bin/git` and `/usr/lib/git-core/git-remote-https`. The sandbox
policy passed at create repeats the same endpoint and rules as `network_policies.github_repository_push`, with the
same two binaries, and sets no filesystem, Landlock or process section (the defaults apply). The credential has
`env_vars: [GITHUB_TOKEN, GH_TOKEN]`, `auth_style: bearer`, `header_name: authorization`.

### Result: clone, edit, push

Through the remote tools only: `git config`, `git clone` (private repo), `read_file`, `edit_file` (appends a
timestamped line to `README.md`), `git commit` and `git push origin HEAD:spike/sandbox-push`. The sandbox's
`git rev-parse HEAD` was `55004239c7a4b0c63b7a8e838368c6bc9d889c55`, and
`gh api repos/wolfeidau/durable-sandbox-scratch/branches/spike/sandbox-push --jq .commit.sha` on the host returned
the same SHA. The proxy log shows `ALLOWED GET .../info/refs`, `POST .../git-upload-pack` and
`POST .../git-receive-pack` under `policy:_provider_durable_gh_spike engine:l7`.

### Placeholder evidence and egress

- `env | grep -i -E 'gh|github|token'` in the agent printed `GITHUB_TOKEN` (an `openshell:resolve:env:...`
  placeholder, not equal to the real token by sha256 comparison) and `DURABLE_WORKER_TOKEN` (the random worker
  token). The real token appeared nowhere in the output.
- `curl https://example.com` and `curl https://api.github.com/user` both fail with exit 7 in about 1 ms. The log
  shows `DENIED example.com [reason:policy_dns_ineligible]` and
  `DENIED /usr/bin/curl -> api.github.com:443 [reason:transparent_tcp_policy_denied]`.
- `git ls-remote` on a different repo (`wolfeidau/durable`) returned a 403 from the proxy: the rules cover one repo.

### Tool users (`-users`, `Dockerfile.users`)

The agent is uid 1500 with `CapEff=0`, `CapBnd=0`, `NoNewPrivs=1` and `Seccomp=2`.

| Attempt | Result |
|---|---|
| Read `/proc/$PPID/environ` (worker) | Works: `DURABLE_WORKER_TOKEN` present |
| `sudo -n -u tool id` | `sudo: The "no new privileges" flag is set, which prevents sudo from running as root.` |
| `setpriv --reuid=1501 ...` | `setpriv: activate capabilities: Operation not permitted` |
| setuid-root copy of `env` | Runs as uid 1500: the setuid bit is ignored under no-new-privs |
| `su tool -c id` | `su: System error` |
| `mount -o remount,hidepid=2 /proc` | `mount: /proc: must be superuser to use mount.` (`/proc` is `rw,nosuid,nodev,noexec`) |
| `unshare -U -r id` | `Operation not permitted` |
| Policy `filesystem` without `/proc` (Landlock) | **Works.** `ls /proc` and `cat /proc/$PPID/environ` give `Permission denied`, and the worker and the tools still run |

So a different uid isn't possible inside the sandbox (no capabilities, no-new-privs). A policy filesystem section
that omits `/proc` stops the agent reading the worker's environment, but also stops it reading `/proc/self`
(no `ps`, no `/dev/fd` process substitution). It isn't a different identity, and the worker could be reached
by other means (same uid). The probe policy used read-only `/usr /lib /etc /bin /sbin /run/openshell-supervisor-ca`
and read-write `/sandbox /tmp /dev`; Landlock `best_effort`.

### Open questions

1. How a session names its repo and branch, and who writes the per-session policy and profile.
2. Token scope: a fine-grained PAT per repository, or a GitHub App installation token, and rotation.
3. Pushes are allowed by policy without an approval. Should `git-receive-pack` be gated by the approval flow?
4. Profile import needs the CLI today; is the SDK gap (no L7 rules on profile endpoints) intended?
