# durable

A small durable harness for a Go coding agent, modeled on the ideas in Pi Durable.
Every step of a run is committed to SQLite before the next one starts, so you can
`kill -9` the agent mid-run, start it again, and it picks up where it stopped.

## Running the agent

`cmd/durable` is a small coding agent built on the harness. It uses Claude by default, or
a model through OpenRouter, and has five tools (`read_file`, `list_files`, `grep`,
`edit_file`, `bash`).

```sh
go build -o durable ./cmd/durable
./durable --api-token-from-1password 'op://<vault>/<item>/<field>'
```

Flags: `--provider` (default `anthropic`), `--protocol` (the provider's default),
`--model` (default `claude-sonnet-5-5` on `anthropic`, required on `openrouter`),
`--effort` (default `medium` on `anthropic-messages`, unset on `openai-responses`),
`--session` (default `default`), `--yes`, `--no-context`, `--version`, `--api-token` and
`--api-token-from-1password`.

- **Providers and protocols:** the provider is where requests go and which key they use.
  The protocol is the wire format, and it is also the format of the stored model replies.

  | Provider | Key | Default protocol | Allowed protocols |
  |---|---|---|---|
  | `anthropic` | SDK defaults | `anthropic-messages` | `anthropic-messages` |
  | `openrouter` | `OPENROUTER_API_KEY` | `openai-responses` | `openai-responses`, `anthropic-messages` |

  ```sh
  ./durable                                                    # anthropic, anthropic-messages
  ./durable --provider openrouter --model openai/gpt-5.6-luna  # openai-responses
  ./durable --provider openrouter --protocol anthropic-messages --model anthropic/claude-sonnet-5-5
  ```

  Every start prints a `[model] <provider> via <protocol>: <model>` line showing the route.
  `openai-responses` is the OpenRouter default for every model, but an `anthropic/*` model
  on it loses Anthropic prompt caching and can cost several times more, so the agent prints
  a warning. Use `--protocol anthropic-messages` for Claude models. The `OPENROUTER_API_KEY`
  variable, `--api-token` or `--api-token-from-1password` supplies the key, and
  `OPENROUTER_BASE_URL` overrides the root URL (without `/v1`). The OpenRouter route
  ignores `ANTHROPIC_*` from the environment, so neither key goes to the wrong service.
  The provider, protocol and OpenRouter model are pinned in `provider.json` next to the
  session database when the session first starts, because stored replies only replay under
  the protocol that wrote them. Changing any of them needs a new `--session`, and a
  mismatched flag is an error. Sessions that predate this feature stay `anthropic`.
  Limits on OpenRouter: server-side compaction exists only on the direct `anthropic`
  provider, so a long session fails at the model's context limit. `anthropic-messages` via
  OpenRouter also leaves out server-side fallbacks, and sends thinking block binding only
  to Claude models, because OpenRouter rejects it on other models. `--effort`
  is sent to `openai-responses` only when set, and a value the model doesn't support fails
  with the provider's error.

- **Credentials:** `--api-token-from-1password` reads the key with the `op` CLI when the
  agent starts. `--api-token` also works, but the key is then visible in the process list
  and shell history. With neither flag, the SDK's defaults apply (`ANTHROPIC_API_KEY`, or
  `ant auth login`). `bash` commands don't inherit `ANTHROPIC_*`, `OPENROUTER_*` or `OP_*` variables, but
  `bash` isn't sandboxed, so an approved command can still reach anything you can.
- **Sessions** live in `~/.durable/sessions/<hash>/session.sqlite`, where the hash covers
  the working directory and `--session`. To resume after a crash or a quit, run the agent
  again in the same directory. Unfinished work continues before the first prompt.
- **Project instructions:** a new session loads `AGENTS.md` files into the system prompt, in
  this order: `~/.durable/AGENTS.md`, then each `AGENTS.md` from the repository root (the
  nearest ancestor with a `.git` entry) down to the working directory. The walk stops at the
  root, and with no repository only the working directory is read. `CLAUDE.md` isn't read, and
  `@path` imports stay as literal text. Files are limited to 32 KiB each and 64 KiB in total,
  counting file content but not the wrapper tags; anything over is cut, marked `[truncated]`
  and reported at startup. The budget is spent from the working directory upward, so the most
  specific files are kept. A project `AGENTS.md` symlink that resolves outside the repository
  is refused, as is anything that isn't a regular file; an in-repository symlink must be
  relative. The `~/.durable` file may be a symlink. Files containing NUL bytes are skipped, and
  a `.git` owned by another user isn't treated as a repository root. The text is pinned in
  `instructions.md` next to the session database when the session first starts, even if no
  prompt is sent and even if the first request fails, so a resume uses the same prompt and
  prints a `[context]` line saying pinned instructions are in use. To reload, start a new
  `--session` or delete the session directory; deleting only `session.sqlite` keeps the pin.
  Sessions that predate this feature keep an empty pin, so `AGENTS.md` isn't loaded for them.
  `--no-context` skips loading for a new session.
  **A cloned repository's `AGENTS.md` goes into the system prompt, so treat it as untrusted
  input, especially with `--yes`.**
- **MCP tools:** a new session connects to the servers in `~/.durable/mcp.json`, which uses
  the `mcpServers` shape from Claude Code. Stdio and streamable HTTP servers work; `sse`
  entries and OAuth don't. A project `.mcp.json` is never read, because a repository must not
  be able to start commands. `env` and `headers` values can use `${VAR}`. An unset variable
  skips that server: it stops a new session from starting, and on a resume it is a warning.
  The same goes for an invalid file. `headers` go only to the configured URL's scheme and host,
  not to a redirect elsewhere. `timeoutSeconds` limits each call (default 10 minutes). A stdio
  server runs in a fresh empty temp folder, removed when it stops, so a cloned repository
  can't influence it (for example `npx` resolving a package from the project). A relative
  `command` no longer resolves in the repository; `"useWorkspaceCwd": true` runs that server
  in the workspace again. This doesn't sandbox the server, which still runs as you. Stdio
  servers don't inherit `ANTHROPIC_*`, `OPENROUTER_*` or `OP_*` unless you pass them this way, and their stderr goes to
  `mcp-<server>.log` next to the session database. Tools are named `mcp__<server>__<tool>`
  and pinned in `mcp-tools.json` when the session first starts, so a resume uses the same list
  without connecting, and a server connects on its first call. A server that fails at session
  start is reported and has no tools in that session. To reload, start a new `--session`.
  Sessions that predate this feature keep an empty pin. Server hints are ignored: every MCP
  call asks unless you use `--yes`, and a call interrupted by a crash, a timeout or a lost
  connection is never retried and is reported as an unknown outcome. Nothing stops a server
  after a crash: one that ignores stdin closing keeps running, and an HTTP call may finish.
  **Tool descriptions and results come from the server, so treat them as untrusted input.**
- **Skills:** a new session loads [Agent Skills](https://agentskills.io/specification) from
  `.durable/skills` then `.agents/skills` in each project directory, nearest first up to the
  repository root, then `~/.durable/skills` and `~/.agents/skills`. The first skill of a name
  wins and a shadowed one gets a warning. `.claude/skills` isn't scanned, and Claude Code reads only that location, so a skill
  meant for both needs a copy. A project skill
  that resolves outside the repository is skipped. The model loads one with the `skill` tool,
  which lists the skills in its description and needs no approval to read, while scripts a
  skill mentions run through `bash` and ask as usual. The catalog is pinned in `skills.json`
  next to the session database when the session first starts, so a resume uses the same tool;
  to reload, start a new `--session`. Sessions that predate this feature keep an empty pin,
  and `--no-context` skips skills for a new session. `/skill:<name> [text]` loads a skill into
  your own prompt, including one with `disable-model-invocation`, which the model never sees.
  `allowed-tools` is ignored. Compaction may drop a loaded skill's instructions, and the model
  can load it again.
  **A cloned repository's skills go into the model's context, so treat them as untrusted
  input, especially with `--yes`.**
- **Approval:** every tool shows the call and asks `y/N`, except built-in tools that declare
  themselves read-only (`read_file`, `list_files`, `grep`).
- **`--yes` skips approval, and `bash` is not sandboxed.** Approval is the only guard, so
  use `--yes` only where the model may run anything you could.
- **Ctrl-C** during a turn (including an approval prompt or a resumed task) cancels the
  task durably: a restart won't resume it. `kill -9` and SIGTERM still resume. At the
  prompt, Ctrl-C or Ctrl-D exits. A second Ctrl-C exits without waiting for the turn:
  the task resumes on restart, after waiting out the session lease.

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
OpenAI-style Responses APIs such as OpenRouter's, and `tools.All` returns the five coding tools scoped to a
workspace root. Its `stateDir` holds crash bookkeeping, so keep it outside the workspace:

```go
toolset, err := tools.All(workspace, filepath.Dir(path))
model := claude.New(anthropic.NewClient(), claude.Config{System: systemPrompt})
```

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
may-still-be-running; Unix only. OS sandboxing is out of scope.

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
terminal, so retrying needs a new request ID. Wrap errors worth retrying (rate limits,
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

**Long sessions.** On the direct Anthropic provider, the adapter turns on server-side compaction on Claude 5 and later.
When a request reaches 150,000 input tokens, the API summarizes the older history into a
compaction block at the start of the reply, and the agent prints `[compacting history]`.
The block is stored in `Raw` like any other content, and the API ignores everything
before it on later requests. Nothing is rewritten, so the transcript stays append-only
and a crash needs no special handling. The whole transcript is still sent, so tokens
drop but request size keeps growing. Every later request must enable compaction, so a
session that has compacted can't continue on an older `--model`.

**Events.** Every state change inserts a row into `events` in the same transaction, so an
event exists only if its change committed. Kinds are `submission` (queued, then each
status change, with `Error` when failed), `message` (a transcript entry, without `Raw`)
`call_started` (the input that actually runs, with `Rewritten` set when a gate changed
it), `call_replayed` (a `ReplaySafe` call running again after a crash) and `call_done` (a
call's result, whether it ran or not, with the text in the `message` event before it). `Options.Observer` is called after each commit with the new events in `Seq`
order. It is live only: events from before `Open` are not replayed. To catch up, call
`Events(ctx, after, limit)` after `Open` and before `Run`, then drop any `Seq` you have
already seen. A crash between commit and delivery loses only the callback, and a failed
delivery is retried on the next commit, if there is one. So `Observer` alone isn't a complete
feed: reconcile with `Events` if you need every event, which gives at-least-once across
restarts. `Observer` runs synchronously on the committing goroutine,
so a slow one slows the agent. It may call `Events` or `History`, but not `Submit`, `Run`
or `Cancel`, which deadlock, and it must not panic. `Event.Call.Input` is the effective input and can hold
secrets a gate added; treat the log accordingly.

**What isn't durable:** an in-flight model call. If the process dies mid-stream the
call is simply made again, which costs tokens but loses nothing.

## Tests

`go test -race ./...` includes real `kill -9` tests in `cmd/durable`. They build the binary, point
it at a fake Anthropic server, kill it mid-`bash` and at an approval prompt, then restart it and
check the tool ran once and the outcome was reported. The harness tests simulate crashes by
cancelling the context at the worst moments, including after a tool's side effect and during a
model call.

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
4. **Persisting partial streams.** Only worth it if you want clients to see partial
   output after a reconnect; commit partials to a separate table and discard them
   when the final message commits.

## Built with AI

This project is built with AI-assisted development workflows using tools including [OpenAI Codex](https://developers.openai.com/codex/), [Claude Code](https://claude.ai/code), and [Cursor](https://cursor.com).

## License

Apache-2.0. See [LICENSE](LICENSE).
