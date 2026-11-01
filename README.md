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

Not included: a TUI, parallel tool calls, subagents, an OS sandbox or usage tracking.
See the [feature matrix](docs/feature-matrix.md).

## Quick start

You need Go 1.27, plus cgo and a C toolchain because of `mattn/go-sqlite3`. There are no released
binaries yet, so build from source.

```sh
go build -o durable ./cmd/durable
./durable --api-token-from-1password 'op://<vault>/<item>/<field>'
```

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

## Extending it

- **Providers:** `anthropic` is the default, and `openrouter` reaches other models over
  `openai-responses` or `anthropic-messages`. The provider, protocol and OpenRouter model are pinned
  per session. See [docs/providers.md](docs/providers.md).
- **Project instructions:** a new session loads `AGENTS.md` files into the system prompt, from
  `~/.durable` and from the repository root down to the working directory. **A cloned repository's
  `AGENTS.md` goes into the system prompt, so treat it as untrusted input, especially with
  `--yes`.** `--no-context` skips loading. See [docs/context.md](docs/context.md).
- **Skills:** a new session loads [Agent Skills](https://agentskills.io/specification) from
  `.durable/skills`, `.agents/skills` and the matching folders in `~/`. **A cloned repository's
  skills go into the model's context, so treat them as untrusted input, especially with `--yes`.**
  See [docs/context.md](docs/context.md#skills).
- **MCP tools:** a new session connects to the servers in `~/.durable/mcp.json`. Every MCP call asks
  unless you use `--yes`, and a stdio server is not sandboxed. **Tool descriptions and results come
  from the server, so treat them as untrusted input.** See [docs/mcp.md](docs/mcp.md).
- **Hooks:** `~/.durable/hooks.json` runs commands before and after tool calls, using the JSON
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

- `mcp` turns MCP server tools into `Tool`s. `mcp.LoadConfig` reads the `mcpServers` file,
  `mcp.NewPool` owns one lazily connected session per server, `Pool.Discover` lists the tools to pin,
  and `Pool.Tools` builds `Tool`s from a pinned list without connecting. Close the pool when done.
  It is separate from `tools` so importing `tools` doesn't link the MCP SDK.
- `hooks` runs `PreToolUse` and `PostToolUse` commands. `hooks.LoadConfig` reads the file and
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
`compacted` (a client-side compaction, with the summary in the database rather than the event) and
`compaction_failed` (a compactor error that didn't stop the run, in `Error`).

`Options.Observer` is called after each commit with the new events in `Seq` order. It is live only:
events from before `Open` are not replayed. To catch up, call `Events(ctx, after, limit)` after
`Open` and before `Run`, then drop any `Seq` you have already seen. A crash between commit and
delivery loses only the callback, and a failed delivery is retried on the next commit, if there is
one. So `Observer` alone isn't a complete feed: reconcile with `Events` if you need every event,
which gives at-least-once across restarts. `Observer` runs synchronously on the committing
goroutine, so a slow one slows the agent. It may call `Events` or `History`, but not `Submit`, `Run`
or `Cancel`, which deadlock, and it must not panic. `Event.Call.Input` is the effective input and
can hold secrets a gate added; treat the log accordingly.

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

This project is built with AI-assisted development workflows using tools including
[OpenAI Codex](https://developers.openai.com/codex/), [Claude Code](https://claude.ai/code), and
[Cursor](https://cursor.com).

## License

Apache-2.0. See [LICENSE](LICENSE).
