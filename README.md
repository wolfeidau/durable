# durable

A small durable harness for a Go coding agent, modeled on the ideas in
[Pi Durable](https://github.com/earendil-works/pi/blob/main/packages/durable/README.md).
Every step of a run is committed to SQLite before the next one starts, so you can
`kill -9` the agent mid-run, start it again, and it picks up where it stopped.

For how it compares with Pi, Pi Durable and Claude Code, see the [feature matrix](docs/feature-matrix.md).

## Features

- **Crash-safe runs:** every step commits to SQLite first, and `kill -9` resumes automatically.
- **No double side effects:** per-tool replay rules, unknown-outcome results and orphaned `bash` cleanup.
- **Single writer:** a lease fences processes, and `synchronous=FULL` survives power loss.
- **Models:** Anthropic and OpenRouter, pinned per session.
- **Extensions:** MCP tools, Agent Skills, `AGENTS.md`, and `PreToolUse` and `PostToolUse` hooks.
- **Embeddable:** a Go library with two interfaces, `Model` and `Tool`, and reusable adapters and tools.
- **Server and web UI:** `durable-server` serves the sessions over a Connect API and a browser UI on
  loopback: create a session, watch the reply stream, approve tool calls and cancel.
- **Sandboxed sessions:** with [OpenShell](https://github.com/ai-openshell/openshell), the server
  runs each session's tools in its own sandbox. The workspace is empty or a clone of a GitHub repo
  and branch, and a repo session loads the clone's `AGENTS.md` and skills.

Not included: a TUI, parallel tool calls, subagents, an OS sandbox for `durable chat`, or usage tracking.
See the [feature matrix](docs/feature-matrix.md).

## Quick start

You need Go 1.27, plus cgo and a C toolchain because of `mattn/go-sqlite3`. Releases are source
only, with no prebuilt binaries for now, so install with Go:

```sh
go install github.com/wolfeidau/durable/cmd/durable@latest
durable --api-token-from-1password 'op://<vault>/<item>/<field>'
```

To build a checkout instead, run `go build -o durable ./cmd/durable`.

To resume after a crash or a quit, run the agent again in the same directory. Unfinished work
continues before the first prompt.

## Using the agent

`cmd/durable` is a small coding agent built on the harness. It uses Claude by default, or a model
through OpenRouter. It has five coding tools (`read_file`, `list_files`, `grep`, `edit_file`,
`bash`), plus a `skill` tool when the session has skills the model can load.

Flags: `--provider` (default `anthropic`), `--protocol` (the provider's default),
`--model` (default `claude-sonnet-5-5` on `anthropic`, required for a new `openrouter` session),
`--effort` (default `medium` on `anthropic-messages`, unset on `openai-responses`),
`--session` (default `default`), `--yes`, `--no-context`, `--version`, `--api-token` and
`--api-token-from-1password`.

- **Credentials:** `--api-token-from-1password` reads the key with the `op` CLI when the agent
  starts. `--api-token` also works, but the key is then visible in the process list and shell
  history. With neither flag, the SDK's defaults apply (`ANTHROPIC_API_KEY`, or `ant auth login`).
  `bash` commands don't inherit `ANTHROPIC_*`, `OPENROUTER_*` or `OP_*` variables, but `bash` isn't
  sandboxed, so an approved command can still reach anything you can.
- **Sessions** live in `~/.durable/sessions/<hash>/session.sqlite`, where the hash covers the
  working directory and `--session`. To resume after a crash or a quit, run the agent again in the
  same directory. Unfinished work continues before the first prompt.
- **Approval:** every tool shows the call and asks `y/N`, except built-in tools that declare
  themselves read-only (`read_file`, `list_files`, `grep`, and `skill`).
- **`--yes` skips approval, and `bash` is not sandboxed.** Approval is the only guard, so use
  `--yes` only where the model may run anything you could. It also covers MCP tools.
- **Turn limit:** a submission fails permanently after 50 model turns (`Options.MaxTurns`, default
  50). The CLI has no flag to change it, and retrying needs a new prompt.
- **Ctrl-C** during a turn (including an approval prompt or a resumed task) cancels the task
  durably: a restart won't resume it. `kill -9` and SIGTERM still resume. At the prompt, Ctrl-C or
  Ctrl-D exits. A second Ctrl-C exits without waiting for the turn: the task resumes on restart,
  after waiting out the session lease.

## Running the server

`durable-server` runs the same agent behind an API and a web UI. It listens on `127.0.0.1:7878` and
keeps one bearer token in `~/.durable/server-token`:

```sh
go install github.com/wolfeidau/durable/cmd/durable-server@latest
durable-server serve --api-token-from-1password 'op://<vault>/<item>/<field>'
durable-server url   # prints the browser link, with the token in the URL fragment
```

- **Web UI:** a `go install` build serves a placeholder page. To embed the UI, build from a checkout
  with `mise run web:build` first, then `go build ./cmd/durable-server`.
- **Models:** `provider` in the config, or `--provider`, `--protocol` and `--model`, set the route for new
  sessions, as in `durable chat`. Existing sessions keep their pin.
- **Sessions:** without a sandbox, a session is a directory on your machine, typed as an absolute
  path, and its tools run as you. It is the same session `durable chat` opens there, and a lease
  hands it between the two.
- **Access:** loopback only by default, and `durable-server rotate-token` replaces the token. See
  [docs/server-access.md](docs/server-access.md).

### Server config

The server reads `~/.durable/config.json` or `config.yaml` (or `.yml`), not both. A missing file means
the defaults. Precedence is default,
then file, then flag, and each flag that overrides a file value logs a warning naming the flag and
the field, never the value. A `--provider` that names a different provider also drops the file's
`protocol`, `model` and `api_key`.

```json
{
  "$schema": "https://raw.githubusercontent.com/wolfeidau/durable/main/schema/durable-server.schema.json",
  "listen": { "addr": "127.0.0.1:7878", "idle_timeout": "5m", "open_timeout": "10s", "heartbeat": "20s" },
  "provider": { "name": "openrouter", "protocol": "anthropic-messages", "model": "anthropic/claude-haiku-5.5", "effort": "medium", "api_key": "op://<vault>/<item>/<field>" },
  "git": { "author": "durable <durable@localhost>", "hosts": { "github": { "token": "op://<vault>/<item>/<field>" } } },
  "environments": {
    "default": { "image": "durable-worker:dev" },
    "go": { "image": "durable-worker-go:dev", "grants": ["go-modules"] }
  }
}
```

The same file as `config.yaml`:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/wolfeidau/durable/main/schema/durable-server.schema.json
listen:
  addr: 127.0.0.1:7878
  idle_timeout: 5m
  open_timeout: 10s
  heartbeat: 20s
provider:
  name: openrouter
  protocol: anthropic-messages
  model: anthropic/claude-haiku-5.5
  effort: medium
  api_key: op://<vault>/<item>/<field>
git:
  author: durable <durable@localhost>
  hosts:
    github:
      token: op://<vault>/<item>/<field>
environments:
  default:
    image: durable-worker:dev
  go:
    image: durable-worker-go:dev
    grants: [go-modules]
```

- **Examples:** [`examples/config`](examples/config) has one file per provider, protocol and model
  combination the integration tests cover, with GitHub tokens from a 1Password PAT or, for local dev,
  the `gh` CLI:

  | File | Provider | Protocol | Model | GitHub token |
  |---|---|---|---|---|
  | `anthropic.yaml` | `anthropic` | `anthropic-messages` | default | `gh` |
  | `openrouter-responses-gpt.yaml` | `openrouter` | `openai-responses` | `openai/gpt-6-luna`, effort `low` | 1Password |
  | `openrouter-responses-glm.yaml` | `openrouter` | `openai-responses` | `z-ai/glm-5.3-flash` | `gh` |
  | `openrouter-messages-claude.yaml` | `openrouter` | `anthropic-messages` | `anthropic/claude-haiku-5.5` | 1Password |
  | `openrouter-messages-glm.yaml` | `openrouter` | `anthropic-messages` | `z-ai/glm-5.3-flash` | `gh` |

- **Effort:** `provider.effort` (or `--effort`) sets reasoning effort for every session the server
  runs, as the CLI's `--effort` does. It isn't pinned, so a change applies when a session next opens.
- **References, never secrets:** `provider.api_key` and `git.hosts.<kind>.token` take an `op://`
  1Password reference (`op://<vault>/<item>/[<section>/]<field>`, checked at startup), `env:NAME`, `file:PATH` (absolute or `~/`, read once at startup with surrounding whitespace trimmed), or, for a git token only, `gh`. A pasted token fails validation,
  and errors never quote the value. Without `api_key`, openrouter reads `OPENROUTER_API_KEY` and
  anthropic uses the SDK's credentials.
- **Schema:** [`schema/durable-server.schema.json`](schema/durable-server.schema.json) is generated
  from the Go types by `mise run gen:schema`. Unknown keys, bad enums and literal secrets are
  errors, and so are an openrouter provider with no model and a git token with no environment.
  Errors exit non-zero before the server listens. The server and `config check` also validate the
  `mcp` and `hooks` files at startup.
- **`durable-server config check`** validates the file, prints warnings, and prints each effective
  setting with its source. It reads only the files, not flags, and never resolves a secret.
  `durable-server config schema` prints the schema.
- **Legacy `environments.json`** is still read, with a deprecation warning, when `config.json`
  defines no environments. If both define them, startup fails.

### Tracing

Setting `otel.endpoint` exports traces over OTLP. Without it tracing is off. For `http/protobuf`, a base URL
without a path gets `/v1/traces` appended, and an explicit path such as `/custom/path` is used as is.

```json
{
  "otel": {
    "endpoint": "https://otel.example.com:4318",
    "protocol": "http/protobuf",
    "headers": { "authorization": "env:OTLP_AUTH" },
    "capture_content": false
  }
}
```

- **Spans:** each run is `durable.run`, then `invoke_agent durable`, which holds a `chat <model>` span
  per model call, an `execute_tool <name>` span per tool call, and `durable.compact` (only when a
  compaction runs or fails) and `durable.hook` where they happen. Spans carry `gen_ai.*` attributes such as `gen_ai.operation.name`,
  `gen_ai.tool.name` and `gen_ai.tool.call.id`.
- **One trace per run, keyed by session:** every span carries the session ID as `gen_ai.conversation.id` and `session.id`, so filter a session by either.
- **Headers:** values are `op://` references, `env:NAME` or `file:PATH`, resolved at startup, and never logged. A
  reference that can't resolve fails startup.
- **Headers file:** `"headers_from": "file:~/.config/otel/headers"` reads a list in the
  `OTEL_EXPORTER_OTLP_HEADERS` format, `key=value,key2=value2` with URL-encoded values. It merges with
  `headers`, and a name set by both fails startup.
- **Content:** `capture_content` is off by default. When on, spans also carry
  `gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.tool.call.arguments` and
  `gen_ai.tool.call.result`. Tool results are taken after `PostToolUse` hooks, so their redaction applies.
  `gen_ai.tool.call.arguments` is the model's original input. A gate-rewritten input is never exported, and a
  replayed rewritten call omits the attribute (`durable.rewritten=true` marks it).
  Prompts and files can still hold secrets, so send them only to a collector you trust.
- **Protocols:** `http/protobuf` (default) or `grpc`. An export failure logs a warning and never
  affects a run.

### Sandboxed sessions

An environment runs the tools of new sessions in an [OpenShell](https://docs.nvidia.com/openshell/)
sandbox, one per session. The model loop, the model key, hooks, approvals and `session.sqlite` stay
on the host; the sandbox runs `durable-worker`, which executes the built-in tools and MCP servers.
You need a running OpenShell gateway and the worker image, built from a checkout:

```sh
mise run image:worker
durable-server serve --sandbox-image durable-worker:dev --git-host-token github=gh
```

`--sandbox-image REF` is shorthand for an environment named `default`. Without any environment the
server creates directory sessions only, and contacts a gateway only to reopen an existing sandboxed
session.

- **Workspaces:** a sandboxed session starts empty, or as a clone of a repo and branch (a URL or
  `github.com/owner/name`). Work comes back through `git push`, which needs no approval of its own.
  A repo session is keyed by repo, branch and name, and an empty one by the client's `request_id`,
  so a retried create returns the same session.
- **Project context:** a repo session pins the clone's `AGENTS.md` and its `.durable/skills` and
  `.agents/skills` when it is created. An empty session gets only the user's `~/.durable` ones.
- **Commit author:** `git.author` or `--git-author "NAME <EMAIL>"`, default
  `durable <durable@localhost>`, is pinned per session.
- **Pinning:** the environment's image and grants are pinned in `sandbox.json` at create, so editing
  or removing an environment never changes an existing session. A session with `sandbox.json` always
  runs in its sandbox, and `durable chat` refuses it.
- **Lifecycle:** the sandbox stops when the session idles and starts when it opens again, keeping
  its workspace. A gateway restart leaves sandboxes in `Error`, which the next open recovers
  ([#90](https://github.com/wolfeidau/durable/issues/90)).
- **Resetting a sandbox:** `durable-server sandbox-delete <session>` deletes it. The next open
  creates a fresh one, and a repo session clones its branch again. It is refused while `serve` runs.
- **Rebuild the worker image after upgrading the server.** New repo sessions need the matching worker.
- **Hooks** still run on the host, but their input comes from a sandbox the agent controls. See
  [docs/hooks.md](docs/hooks.md).

#### Network decisions

Network decisions come from the gateway's log, which keeps only recent lines. The built-in OTLP
receiver and `~/.durable/telemetry.db` were removed, and longer retention will come from a separate
service. A `telemetry` key in `config.json` now fails startup, so remove it, along with the
`[openshell.gateway.otlp.ocsf]` block in `gateway.toml`. `~/.durable/otlp-headers` and
`~/.durable/telemetry.db` can be deleted. The `ListNetworkEvents` API call keeps its shape: its
`coverage` marks the source bounded and not resumable, and it issues no cursor.

#### Git host credentials

GitHub is the only git host so far. Configure either a static token or a GitHub App (mutually exclusive).

- Static token: `--git-host-token github=<source>` takes an `op://` reference, `env:NAME`, `file:PATH` or `gh`, which runs
  `gh auth token`. **`gh` is for local dev only:** its token usually reaches every repo your login can. Without it, `git.hosts.github.token` in the config applies, then
  `DURABLE_GITHUB_TOKEN`. `GITHUB_TOKEN` and `GH_TOKEN` are never read.
- GitHub App (requires OpenShell gateway `>= v0.1.10`): configure `git.hosts.github.app` in `~/.durable/config.yaml` or `config.json`:

  ```yaml
  git:
    hosts:
      github:
        app:
          client_id: Iv23li...
          installation_id: 12345678 # optional: omit to discover per repository
          private_key: op://<vault>/<item>/<field> # op://, env:NAME or file:PATH
  ```

  OpenShell's gateway holds the RSA private key, signs app JWTs, and mints and rotates short-lived installation tokens scoped strictly to each session's single repository ID and grant permissions (so even GraphQL requests cannot cross repositories). At startup the server verifies the key and app with `GET /app` and checks the installation and required permissions when `installation_id` is set.
- At startup the server checks static tokens with `GET /user` and logs the login, kind and scopes,
  never the token. Bad credentials fail startup. Sources are read again on each sandbox create or
  start, so a rotated token or private key takes effect without a restart.
- Credentials stay on the server and OpenShell gateway; the sandbox sees only a placeholder, and each session's policy
  limits git to its own repo. When using a static token, **use a fine-grained token** scoped to the repos durable should reach;
  a classic or `gh` token usually reaches every repo you can. Minimum repository permissions (for both fine-grained tokens and GitHub Apps):

  | Permission | Access | Needed for |
  |---|---|---|
  | Metadata | Read | Always (GitHub adds it automatically) |
  | Contents | Read and write (or Read for read-only apps) | Clone and push |
  | Pull requests | Read and write | The `github-api` grant: open, edit and comment on PRs |
  | Issues | Read and write | The `github-api` grant: issues, comments and labels |
  | Checks / Actions | Read (optional) | With a GitHub App + `github-api`, requested automatically when granted for reading CI status |

- With no token or app, or when a GitHub App is not installed on the target repository, repo sessions clone public repos anonymously (without `github-api`) and `git push` fails. Repo access is
  fixed at create, so changing credentials later doesn't upgrade an existing session's pin.
- For static tokens, the server keeps one OpenShell provider and profile, `durable-github`, shared across sessions. To
  remove them: `openshell provider delete durable-github`, then `openshell profile delete durable-github`.
  For GitHub Apps, the server keeps one profile `durable-github-app` and one provider per repository and permission scope (`durable-gh-<repoID>-<hash>`). Scoped providers are not deleted automatically and each stores a copy of the app private key in OpenShell's credential backend; remove unused ones with `openshell provider delete <name>` (especially after rotating an app key).

#### Environments and grants

An environment pairs a worker image with named network grants and an approval mode.
`CreateSession` takes an `environment`; empty means `default`, or the only one when there is
exactly one. A sandbox denies all egress except git to its own repo, the hosts of its HTTP MCP
servers, and its grants.

| Image | Built by | Has |
|---|---|---|
| `durable-worker:dev` | `mise run image:worker` | bash, git, curl, `gh`, `mise` |
| `durable-worker-go:dev` | `mise run image:worker-go` | the above, plus Go and golangci-lint |

| Grant | Allows |
|---|---|
| `go-modules` | `/usr/local/go/bin/go`, or Go installed by mise, GET on `proxy.golang.org`, `sum.golang.org`, and the proxy's redirects to `storage.googleapis.com/proxy-golang-org-prod`. Covers `go mod`, `build`, `test` and `golangci-lint run` |
| `mise` | `/usr/local/bin/mise` read-only downloads (GET, plus HEAD on GitHub) from GitHub release assets, `nodejs.org/dist` and `dl.google.com/go`, plus its update check at `mise.jdx.dev/releases.tsv` |
| `mise-resolve` | `/usr/local/bin/mise` GET lookups of versions and attestations at `mise-versions.jdx.dev` (`/data`, `/api/github/repos/*/*/releases` and `/attestations`), and Sigstore's trust root at `tuf-repo-cdn.sigstore.dev` to verify them, so it can add, upgrade and re-lock tools. Needs `mise`; telemetry stays denied |
| `npm-registry` | bun installed by mise (`/tmp/mise/data/installs/bun/*/bin/bun`) GET on `registry.npmjs.org`. Read-only, so no publish |
| `github-api` | `/usr/local/bin/gh` to open and edit PRs and issues, comment, label and read CI status on the session's repo. Repo sessions with a GitHub token or GitHub App only |

- **Approvals:** `"approvals": "ask"` (default) or `"auto"`. With `auto`, tool calls in the
  environment's sessions run without asking, as `--yes` does for the CLI; `PreToolUse` hooks can
  still deny or ask. It is read on each open, not pinned, and directory sessions never auto-approve.
  `auto` with `github-api` lets a session push and open PRs with no human in the loop, so startup
  warns about it.
- **`github-api` never merges or reviews,** and denies releases, contents, settings and workflow
  dispatch. GraphQL rules check only the operation, not the repo, so the token's scope is the real
  limit. Give `gh` the repo name in lower case. `gh run view --log` fails because logs redirect to
  hosts the grant doesn't allow.
- **`mise`:** the image sets `MISE_LOCKED=1`, so installs come from `mise.lock`, but the model can
  unset it. Anything run under `mise exec` shares the grant, `curl` included. The image ships mise
  2026.11.3, which reads lockfile formats up to 4. A lockfile from a newer mise can fail to read, so
  lock with that version, e.g. `mise exec mise@2026.11.3 -- mise lock --platform linux-arm64,linux-x64`,
  and set a top-level `min_version = "2026.11.3"` in `mise.toml` to make a mismatch fail with a clear error.
  With `mise-resolve`, to add or upgrade tools run `MISE_LOCKED=0 mise use <tool>@<version>` (or
  `mise up --bump`), then `mise lock --platform linux-arm64,linux-x64`, and commit `mise.toml` and
  `mise.lock`. Grants match binaries by path, and mise installs under writable `/tmp`, so the
  new grants are read-only.
- **Auditing grants:** `durable-server sandbox-audit <session>` lists the network requests a
  sandbox made and which grant allowed or denied each, including `NET:REFUSE` rows for DNS and
  bypass refusals. It reads the gateway's recent log lines, so the summary shows the span it
  covers. Check it after granting something new; `--denied` shows only refusals and `--since` sets
  how far back to read (default `24h`).
- **Go image:** `HOME` is the workspace `/sandbox`, and only `/tmp` and `/sandbox` are writable, so
  caches live under `/tmp`. `GOPROXY` has no `direct`, so private modules fail, and
  `GOTOOLCHAIN=local` fails a `go` line newer than the image's Go.
- **MCP servers** run in the sandbox; see [docs/mcp.md](docs/mcp.md#sandboxed-sessions).

## Extending it

- **Providers:** `anthropic` is the default, and `openrouter` reaches other models over
  `openai-responses` or `anthropic-messages`. The provider, protocol and OpenRouter model are pinned
  per session. See [docs/providers.md](docs/providers.md).
- **Project instructions:** a new session loads `AGENTS.md` files into the system prompt, from
  `~/.durable` and from the repository root down to the working directory. **A cloned repository's
  `AGENTS.md` goes into the system prompt, so treat it as untrusted input, especially with
  `--yes`.** `--no-context` skips loading. A sandboxed repo session reads them from its clone. See
  [docs/context.md](docs/context.md).
- **Skills:** a new session loads [Agent Skills](https://agentskills.io/specification) from
  `.durable/skills`, `.agents/skills` and the matching folders in `~/`. **A cloned repository's
  skills go into the model's context, so treat them as untrusted input, especially with `--yes`.**
  See [docs/context.md](docs/context.md#skills).
- **MCP tools:** a new session connects to the servers in `~/.durable/mcp.json` (or `.yaml`). Every MCP call asks
  unless you use `--yes`, and a stdio server is not sandboxed. **Tool descriptions and results come
  from the server, so treat them as untrusted input.** See [docs/mcp.md](docs/mcp.md).
- **Hooks:** `~/.durable/hooks.json` (or `.yaml`) runs commands before and after tool calls, using the JSON
  contract from Claude Code's hooks. **Hooks run as you and aren't sandboxed, and they aren't a
  security boundary:** an approved `bash` command can edit `~/.durable/hooks.json`, and the change
  applies at the next start. See [docs/hooks.md](docs/hooks.md).

## Wiring it into your agent

Implement two interfaces around what you already have:

```go
type Model interface {
    Generate(ctx context.Context, history []Message, tools []ToolSpec) (Message, error)
}

type Tool interface {
    Spec() ToolSpec
    Replay() Replay // ReplaySafe for reads, ReplayNever for anything with side effects
    Execute(ctx context.Context, call ToolCall) (string, error)
}
```

Then replace your in-memory loop:

```go
// Open does not expand "~" or create directories; do both before calling it.
path := filepath.Join(home, ".myagent", "sessions", cwdHash, "session.sqlite")
h, err := durable.Open(ctx, path, durable.Options{
    Model: myModel,
    Tools: []durable.Tool{readTool, editTool, bashTool},
})
defer h.Close()

h.Run(ctx) // finish anything a previous crashed process left behind

sub, err := h.Prompt(ctx, requestID, userInput) // requestID: a UUID per input, or a webhook delivery ID
```

The agent's own parts are public, so you can reuse them instead of writing your own.
`claude.New` returns a `Model` for Anthropic's Messages API, `responses.New` returns one for
OpenAI-style Responses APIs such as OpenRouter's, and `tools.All` returns the five coding tools
scoped to a workspace root. Its `stateDir` holds crash bookkeeping, so keep it outside the
workspace:

```go
toolset, err := tools.All(workspace, filepath.Dir(path))
model := claude.New(anthropic.NewClient(), claude.Config{System: systemPrompt})
```

Three more packages hold the extensions, and each works with any `Tool` list:

- `mcp` turns MCP server tools into `Tool`s. `mcp.ParseConfig` parses the `mcpServers` file,
  `mcp.NewPool` owns one lazily connected session per server, `Pool.Discover` lists the tools to pin,
  and `Pool.Tools` builds `Tool`s from a pinned list without connecting. Close the pool when done.
  It is separate from `tools` so importing `tools` doesn't link the MCP SDK.
- `hooks` runs `PreToolUse` and `PostToolUse` commands. `hooks.ParseConfig` parses the file and
  `hooks.New` returns a `Runner`. `Runner.Gate(base)` returns a `ToolGate` for `Options.Gate`, and
  `Runner.Wrap(tools)` wraps the tools for `Options.Tools`.
- `skills` finds [Agent Skills](https://agentskills.io/specification). `skills.Discover` scans
  sources, and `skills.Tool` returns the read-only `skill` tool, or nil when no skill is visible to
  the model. `skills.Expand` loads a skill into your own prompt text.

## How it works

Five tables in one database file per session: `submissions`, `messages`,
`tool_calls`, an append-only `events` log, and a one-row `lease`.

**One loop, no recovery path.** `drive` reads the current state, takes exactly
one committed step, and repeats:

| State in the database                         | Next step                                  |
|-----------------------------------------------|--------------------------------------------|
| submission `queued`                           | append user message, mark `running`        |
| latest assistant message has unfinished calls | advance the first unfinished call          |
| otherwise                                     | call the model, commit reply + call intents |
| reply has no tool calls                       | same commit marks the submission `done`    |

After a crash the database holds the last committed step, so the same loop picks
the right next step without knowing a crash happened.

**Tool calls go pending → started → done.** The model's reply and the `pending`
rows for its calls commit together. Before executing, the call is marked `started`;
the result and the tool message commit together afterwards. A call found in
`started` on resume crashed mid-flight, so its outcome is unknown:

- `ReplaySafe` tools run again.
- `ReplayNever` tools get an error result (`UnknownOutcome`) telling the model to
  check the current state before retrying. The model is good at this: it'll
  `git status` or re-read the file.

`Options.Gate` decides each `pending` call before it is marked `started`: allow, deny
(recording its `Reason`, or `Denied`), or ask, and it may rewrite the input. `Options.Ask`
is the human yes/no. With no `Gate`, every call goes to `Ask`, and runs if `Ask` is also nil.
`false` from `Ask` records a `Declined` error result and the model carries on.

A `Gate` that asks while `Ask` is nil, or returns an unknown `Action`, denies the call (fail
closed), and the zero `Decision` asks. A rewrite that isn't valid JSON denies the call with
`InvalidRewrite`, because retrying can't fix it. Any other gate error, or a cancelled `Run`,
leaves the call `pending`, so the next `Run` decides again unless `Cancel` records its result.
Neither `Gate` nor `Ask` runs for calls already `started`.

`Options.DeferApprovals` makes a call that needs asking durable instead of blocking on `Ask`.
The harness commits an `approvals` row and an `approval_requested` event, and `Run` returns
`ErrAwaitingApproval` with the submission still `running`. `PendingApproval` returns the waiting
request, `Decide(ctx, seq, allow, by)` records the answer, and the next `Run` continues. An answer
can land before `Run` returns, so check `PendingApproval` before waiting for one. The call
runs the input stored with the approval and the `Gate` is not run again. Repeating an answer is a
no-op, and a different one returns `ErrApprovalDecided`. `Cancel`, or a tool that has vanished,
closes the approval as `cancelled`. It wins over `Ask`; a parked call reopened with only `Ask`
asks then, and with neither it is denied, never allowed. `by` is attribution, not authentication.

Rewritten input is committed with `started`, so replay and `Recover` use it. The stored
assistant message keeps the model's original arguments, because provider content must replay
verbatim. The call's result therefore carries the `Rewritten` note, including after a crash or
`Cancel`. The note leaves out the new input, because a gate may have added secrets.

`ReadOnlyGate` allows tools that implement the optional `ReadOnlyTool` interface
(`ReadOnly() bool`) and asks for the rest. Wrappers around a tool must forward `ReadOnly`, as
with `Recover`. `OpenDB` rejects duplicate tool names.

Tool call IDs are stable once committed, so tools that hit external APIs can pass
`call.ID` as an idempotency key.

A tool whose work can outlive a crash may also implement the optional `Recoverer`
interface (`Recover(ctx, call) string`). Before recording `UnknownOutcome` for a
`started` `ReplayNever` call, and before `Cancel` records `CancelledUnknown` for one,
the harness calls `Recover`, which stops what is left and returns a note appended to
the result. `Recover` must be idempotent: a crash can land after it runs but before
the result commits. If `ctx` ends during `Recover`, the call stays `started`. Wrappers
around a tool must forward `Recover`, because embedding `Tool` does not promote it.

**Orphaned bash commands.** `kill -9` or a forced exit leaves the command's process
group running, so by the time the model sees the unknown outcome the command may still
be changing state. The bash tool therefore starts each command holding an `flock` on a
state file (`<session dir>/bash/<sha256 of call ID>`, outside the workspace), passes the
descriptor to the command, and records the process group ID in the file. The kernel
drops the lock when the last holder exits, so a held lock proves the command is alive.
On recovery, an unlocked file means the command is dead and nothing is signalled (group
IDs get reused). A held lock means `SIGKILL` goes to the recorded group, and the result
says the command was still running and has been stopped. If it cannot be stopped, or
the recorded ID is not usable, the result says the command may still be running.
Limits: a descendant that closes the inherited descriptor and leaves the group escapes
detection; a crash between starting the command and writing the ID is reported as
may-still-be-running; Unix only. OS sandboxing is deferred; see
[#9](https://github.com/wolfeidau/durable/issues/9).

**Exactly-once input.** `submissions.request_id` is unique. Submitting an ID twice
returns the first submission.

**Fenced single writer.** `Open` takes the lease (waiting out a crashed owner's
TTL, 10s by default). Every commit's first statement re-checks ownership, so a
process that was paused and lost its lease gets `ErrLeaseLost` instead of writing
over the new owner. `Close` releases the lease for instant handover; `Detach` closes
without releasing it, for exits where a run may still be stopping. Losing the
lease also cancels the run: `Run` cancels the context it hands to tools and returns
`ErrLeaseLost`, so tools must honour `ctx`. This is cooperative. A tool that ignores
`ctx` keeps running, and detection can take up to `LeaseTTL/3` after takeover, so a
new owner may briefly overlap with it. `Run` is also serialised within a process, so
overlapping calls wait for each other instead of executing the same tool twice.

**Model failures.** An error from `Generate` fails the submission at once, with the
error text in `Submission.Error`, and `Run` moves on to the next one. `failed` is
terminal, so retrying needs a new request ID. A submission also fails once it has made
`MaxTurns` model calls (default 50), with `stopped after 50 model turns` in `Submission.Error`.
Wrap errors worth retrying (rate limits,
outages, network faults) with `durable.Transient`: they are retried up to
`ModelAttempts` times (default 4) with `ModelBackoff` doubling between tries (default
1s, so 1s, 2s, 4s). If they still fail, `Run` returns the error and the submission
stays `running`, so an outage pauses work and the next `Run` resumes it. Cancelling
the context never counts as a failure: the submission stays `running` and the next
`Run` picks it up.

**Cancellation.** `Cancel(ctx, requestID)` moves a submission to the terminal
`cancelled` status, so `Run` never resumes it. For a running submission it records an
error result for every unfinished call of the latest assistant message
(`CallCancelled` for `pending`, `CancelledUnknown` for `started`) and appends a user
note (`RequestCancelled`), so the history stays valid for the model API. Done, failed
and cancelled submissions are returned unchanged. `Cancel` waits for an in-progress
`Run` to stop, so cancel that `Run`'s context first.

**Changed prompts or tools.** Stored thinking blocks are bound to the system prompt and
tool list that produced them, so upgrading the binary can invalidate them. On Claude 5
and later the adapter sends `prefix_mismatch_behavior: "drop_block"`, so the API drops
those blocks and the session carries on without that reasoning, instead of failing with
a 400. It also restarts the prompt cache.

**Long sessions.** On the direct Anthropic provider, the adapter turns on server-side compaction on
Claude 5 and later. The trigger is the API's default, currently 150,000 input tokens per the API,
and this repo doesn't set it. When a request reaches it, the API summarizes the older history into a
compaction block at the start of the reply, and the agent prints `[compacting history]`. The block
is stored in `Raw` like any other content, and the API ignores everything before it on later
requests. Nothing is rewritten, so the transcript stays append-only and a crash needs no special
handling. The whole transcript is still sent, so tokens drop but request size keeps growing. Every
later request must enable compaction, so a session that has compacted can't continue on an older
`--model`.

On OpenRouter the agent compacts on the client instead: when the
context nears the model's window (from `/v1/models`) or the provider reports an overflow, it
summarises older messages with the session's model and keeps the recent ones. The transcript is not
rewritten; see [Providers](docs/providers.md#compaction).

**Events.** Every state change inserts a row into `events` in the same transaction, so an
event exists only if its change committed. Kinds are `submission` (queued, then each
status change, with `Error` when failed), `message` (a transcript entry, without `Raw`),
`call_started` (the input that actually runs, with `Rewritten` set when a gate changed
it), `call_replayed` (a `ReplaySafe` call running again after a crash), `call_done` (a
call's result, whether it ran or not, with the text in the `message` event before it),
`compacted` (a client-side compaction, with the summary in the database rather than the event),
`compaction_failed` (a compactor error that didn't stop the run, in `Error`),
`approval_requested` (a call parked for `Decide`, with the input that runs if allowed) and
`approval_decided` (the answer in `Approval` and who gave it in `DecidedBy`, or `cancelled` when
the call finished without one).

`Options.Observer` is called after each commit with the new events in `Seq` order. It is live only:
events from before `Open` are not replayed. To catch up, call `Events(ctx, after, limit)` after
`Open` and before `Run`, then drop any `Seq` you have already seen. A crash between commit and
delivery loses only the callback, and a failed delivery is retried on the next commit, if there is
one. So `Observer` alone isn't a complete feed: reconcile with `Events` if you need every event,
which gives at-least-once across restarts. `Observer` runs synchronously on the committing
goroutine, so a slow one slows the agent. It may call `Events` or `History`, but not `Submit`, `Run`
or `Cancel`, which deadlock, and it must not panic. `Event.Call.Input` is the effective input and
can hold secrets a gate added; treat the log accordingly.

**Live deltas.** `Options.OnDelta` receives model text (`DeltaText`) and `bash` output
(`DeltaToolOutput`) as they happen, and `DeltaReset` before every model attempt. Deltas are a
best-effort live view and are never persisted. A client drops partial text on `DeltaReset`, the
assistant `message` event or a terminal `submission` event, drops partial tool output on
`call_done`, and rebuilds from `Events` after a reconnect. `OnDelta` runs synchronously, possibly on a tool's goroutine, so it must not block, call
the harness or panic. A call with a matching `PostToolUse` hook has no live output, because the hook
may redact or withhold the result.

**What isn't durable:** an in-flight model call. If the process dies mid-stream the
call is simply made again, which costs tokens but loses nothing.

## Tests

`go test -race ./...` includes real `kill -9` tests in `cmd/durable`. They build the binary, point
it at a fake Anthropic server, kill it mid-`bash` and at an approval prompt, then restart it and
check the tool ran once and the outcome was reported. The harness tests simulate crashes by
cancelling the context at the worst moments, including after a tool's side effect and during a
model call.

The server's real-binary tests in `cmd/durable-server` cover the API, approvals, idle close and
network faults. `mise run test:openshell` runs sandboxed sessions, including a repo clone, against a
local OpenShell gateway. Plain `go test` skips them.

## Docs

- [Feature matrix](docs/feature-matrix.md): durable compared with Pi, Pi Durable and Claude Code.
- [Providers](docs/providers.md), [context](docs/context.md), [MCP](docs/mcp.md) and
  [hooks](docs/hooks.md).
- [Server access](docs/server-access.md).

## Next steps, roughly in order of payoff

1. **Bash replay.** Most of a coding agent's risk is in `bash`. Keep it
   `ReplayNever` (orphaned commands are already stopped on recovery), and consider
   recording the command's working-tree state (e.g. a `git stash create` hash) in the
   `started` commit so the unknown-outcome message can say exactly what changed.
2. **Steering.** Let `Submit` arrive while a run is in progress and have the loop
   check for queued input between tool calls (Pi's "queue modes").
3. **Durable subagents / tasks.** Model a subagent as another submission (or another
   session file) that the parent tool call waits on; the parent's call ID owns it,
   so cancelling one cancels the other.
4. **Persisting partial streams.** Live deltas are not stored. Only worth it if you want
   clients to see partial output after a reconnect; commit partials to a separate table and
   discard them when the final message commits.

## Built with AI

This project is built with AI-assisted development workflows using tools including
[OpenAI Codex](https://developers.openai.com/codex/), [Claude Code](https://claude.ai/code), and
[Cursor](https://cursor.com).

## License

Apache-2.0. See [LICENSE](LICENSE).
