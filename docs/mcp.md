# MCP tools

Connect Model Context Protocol servers and expose their tools to the model.

**Tool descriptions and results come from the server, so treat them as untrusted input.**

## Configuration

A new session connects to the servers in `~/.durable/mcp.json`, which uses the `mcpServers` shape
from Claude Code.

- Stdio and streamable HTTP servers work. `sse` entries and OAuth don't.
- A project `.mcp.json` is never read, because a repository must not be able to start commands.
- `env` and `headers` values can use `${VAR}`.
- An unset variable skips that server: it stops a new session from starting, and on a resume it is
  a warning. The same goes for an invalid file.
- `headers` go only to the configured URL's scheme and host, not to a redirect elsewhere.
- `timeoutSeconds` limits each call (default 10 minutes).

## Stdio servers

- A stdio server runs in a fresh empty temp folder, removed when it stops, so a cloned repository
  can't influence it (for example `npx` resolving a package from the project).
- A relative `command` no longer resolves in the repository. `"useWorkspaceCwd": true` runs that
  server in the workspace again.
- This doesn't sandbox the server, which still runs as you.
- Stdio servers don't inherit `ANTHROPIC_*`, `OPENROUTER_*` or `OP_*` unless you pass them through
  `env`.
- Their stderr goes to `mcp-<server>.log` next to the session database.

## Naming and pinning

Tools are named `mcp__<server>__<tool>` and pinned in `mcp-tools.json` when the session first
starts.

- A resume uses the same list without connecting, and a server connects on its first call.
- A server that fails at session start is reported and has no tools in that session.
- To reload, start a new `--session`.
- Sessions that predate this feature keep an empty pin.

## Approval and crashes

- Server hints are ignored: every MCP call asks unless you use `--yes`.
- A call interrupted by a crash, a timeout or a lost connection is never retried and is reported as
  an unknown outcome.
- Nothing stops a server after a crash: one that ignores stdin closing keeps running, and an HTTP
  call may finish.
