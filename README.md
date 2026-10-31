# durable

A small durable harness for a Go coding agent, modeled on the ideas in Pi Durable.
Every step of a run is committed to SQLite before the next one starts, so you can
`kill -9` the agent mid-run, start it again, and it picks up where it stopped.

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
h, err := durable.Open(ctx, "~/.myagent/sessions/<cwd-hash>/session.sqlite", durable.Options{
    Model: myModel,
    Tools: []durable.Tool{readTool, editTool, bashTool},
})
defer h.Close()

h.Run(ctx) // finish anything a previous crashed process left behind

sub, err := h.Prompt(ctx, requestID, userInput) // requestID: a UUID per input, or a webhook delivery ID
```

## How it works

Four tables in one database file per session: `submissions`, `messages`,
`tool_calls`, and a one-row `lease`.

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

Tool call IDs are stable once committed, so tools that hit external APIs can pass
`call.ID` as an idempotency key.

**Exactly-once input.** `submissions.request_id` is unique. Submitting an ID twice
returns the first submission.

**Fenced single writer.** `Open` takes the lease (waiting out a crashed owner's
TTL, 10s by default). Every commit's first statement re-checks ownership, so a
process that was paused and lost its lease gets `ErrLeaseLost` instead of writing
over the new owner. `Close` releases the lease for instant handover.

**What isn't durable:** an in-flight model call. If the process dies mid-stream the
call is simply made again, which costs tokens but loses nothing.

## Tests

`go test -race ./...` simulates crashes by cancelling the context at the worst
moments: after an unsafe tool's side effect, after a safe tool's side effect, and
during a model call. Others cover lease exclusion, stale-owner fencing, duplicate
request IDs, and queue ordering.

## Next steps, roughly in order of payoff

1. **Streaming to clients.** Add an observer called after each commit (or have
   clients poll `messages` by `seq`). Because commits are the only state changes,
   "send everything after seq N" gives any number of clients a consistent, resumable
   view: terminal, web, phone.
2. **Bash replay.** Most of a coding agent's risk is in `bash`. Keep it
   `ReplayNever`, and consider recording the command's working-tree state (e.g. a
   `git stash create` hash) in the `started` commit so the unknown-outcome message
   can say exactly what changed.
3. **Steering.** Let `Submit` arrive while a run is in progress and have the loop
   check for queued input between tool calls (Pi's "queue modes").
4. **Durable subagents / tasks.** Model a subagent as another submission (or another
   session file) that the parent tool call waits on; the parent's call ID owns it,
   so cancelling one cancels the other.
5. **Compaction.** When history gets long, commit a summary message plus a marker
   row, and have `loadHistory` start from the latest marker.
6. **Persisting partial streams.** Only worth it if you want clients to see partial
   output after a reconnect; commit partials to a separate table and discard them
   when the final message commits.
