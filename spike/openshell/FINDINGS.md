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
