## Public repository

This repository is public. Keep secrets and private details out of commits, code, tests, issues and pull requests, including API keys, 1Password references (vault, item or field names), account details and private paths. Use placeholders such as `op://<vault>/<item>/<field>`.

When running something that needs a credential, keep it out of command output and logs: pass it to the one command that needs it, and never print, write or commit it. Tool errors can echo a secret reference, so silence or redact their stderr.

## Verification

Fast loop, skipping the real-binary tests in `cmd/durable`, which take about 25s:

```sh
go test . ./claude ./internal/... ./tools/...
```

Before finishing, after your last edit:

```sh
go vet ./... && go test -race -count=1 ./...
mise exec -- golangci-lint run ./...
mise run check:tidy
```

After changing `web/` or `api/`, also run this, which needs Bun from `mise install`:

```sh
mise run check:web
```

CI also race-checks the real binary, which `go test -race` alone doesn't. Run this after changing concurrency in `cmd/durable` or anything it calls:

```sh
GOFLAGS=-race GORACE=atexit_sleep_ms=0 go test -race -count=1 ./cmd/durable
```

CI skips the 50-watcher contention case, the only check that a stalled watcher doesn't slow a run at that scale. Run it after changing the watch fan-out or session event streaming in `cmd/durable-server`:

```sh
mise run test:contention
```

The sandboxed run executes `./cmd/durable` in Docker with egress limited to OpenRouter and GitHub through the proxy allowlist in `e2e/squid.conf`. Set `OPENROUTER_API_KEY` in the environment to include the live tests. On failure the task prints the proxy's denied requests:

```sh
mise run test:e2e
```

## Guiding principles

- Validate upfront. Anything that can be checked before a session starts, such as config, MCP and hook files or provider settings, is checked when `durable-server` starts and by `durable-server config check`, and the server exits before listening when a check fails. A user should find a config problem at startup, not when opening a session. Sessions still re-check files that can change while the server runs.

## Durability invariants

- A state change and its events commit in the same transaction.
- `raw_events` in `telemetry.db` is the system of record for sandbox network events, because the OpenShell gateway keeps none. It archives the full OTLP log record (resource, attributes, body, timestamps) as protojson for every received record, including gaps, other OCSF classes and records with no sandbox, for `telemetry.retention`, so a missed field or an extended event can be re-derived with `otlp.DecodeRaw`. Parsed tables like `decisions` and `daily` are derived from it, and its `uid` is the dedup gate for them.
- `migrate` only runs `CREATE TABLE IF NOT EXISTS`, so a new table reaches existing sessions but a new or changed column doesn't.
- The database pool has one connection, so close a cursor before running another query or calling a callback.
- Wrappers around a tool must forward the optional interfaces `Recoverer` and `ReadOnlyTool`, because embedding `Tool` doesn't promote them.
- The system prompt and tool order must not change within a session. They feed the prompt cache and stored thinking blocks.
- MCP tools are pinned per session, and server hints never change replay or approval, because a hint is only the server's claim.
- The provider, protocol and OpenRouter model are pinned per session in `provider.json`, because stored `Raw` content is specific to its protocol.
- Hook failures must deny or withhold, never return a gate error, because an error leaves the call pending and blocks the queue.
- The skill catalog is pinned per session, for the same reason as the tool order: it feeds the `skill` tool's description and so the prompt prefix.
- A model request is built from `loadContext`, never `loadHistory`.
- Compaction never rewrites `messages`, and `History` stays the full transcript.
- An approved call runs its stored input and is never re-gated, and `recordResult` closes any pending approval, so a finished call leaves no stale prompt.
- Deltas (`Options.OnDelta`) are never durable: they add no events and no rows, and the committed message and `call_done` stay authoritative.
- Live tool output must not bypass `PostToolUse` hooks, because a hook can redact or withhold the result. `hookTool.Execute` decides before the tool runs and passes `durable.WithoutToolOutput(ctx)` when any hook matches.

## Code conventions

### Errors

Return errors up the stack; log only at the top level (the command `Run` function or the HTTP handler boundary). Never log an error and then return it — it will be logged twice.

Wrap errors from other packages with context using `%w`. Errors from the same package may be returned as they are when wrapping would add nothing:

```go
return fmt.Errorf("failed to parse base URL: %w", err)
```

Do not add error handling for impossible cases. Trust the compiler and internal invariants.

### Logging

Use `slog` throughout. Pass the logger via `Globals.Logger` rather than calling `slog.Default()` in business logic. Reserve `slog.Default()` for package-level handlers (middleware, telemetry) where dependency injection is impractical.

Use `InfoContext` / `ErrorContext` inside HTTP handlers so trace IDs propagate automatically.

```go
logger.InfoContext(r.Context(), "access", slog.String("method", r.Method), ...)
```

### Contexts

Pass `context.Context` as the first argument to every function that does I/O, makes external calls, or should respect cancellation. Do not store contexts in structs. Use the context for both cancellation and OTel trace propagation.

### Package names

Lowercase, single word, descriptive of the domain — not the layer. Examples: `oidc`, `middleware`, `telemetry`, `tokens`, `commands`. Avoid generic names like `util`, `helpers`, `common`.

### Comments

Only comment the **why**, never the **what**. No docstrings for obvious functions. A one-line comment is the maximum.

### Web layout

`web/src` is grouped by feature, and tests sit next to their modules:

- `lib/` holds helpers with no components. It imports only `gen/` and `lib/`.
- `ui/` holds shared components and hooks. It imports `lib/` and `ui/`.
- `sessions/`, `server/` and `session/` (with `transcript/`, `report/` and `network/`) are features. A feature imports `lib/`, `ui/`, `gen/` and its own subtree, never another feature. Something two features need moves to `lib/` or `ui/`.
- `shell/` is the app shell (client, `App`, `Topbar`, `TokenInput`). Like `main.tsx` it may import features, and only `main.tsx` imports it.
- `main.tsx` mounts the shell.
- CSS lives next to its feature, and only `main.tsx` imports it, in cascade order.

`mise run check:web:layout` enforces this, and `check:web` runs it.

## Testing

Use `github.com/stretchr/testify/require` for assertions. Prefer `require` over `assert` so test functions fail fast on the first unmet condition.

```go
require.NoError(t, err)
require.Equal(t, http.StatusCreated, resp.StatusCode)
```

- Fake models must also cover nil or empty input, not just `{}`.
- Every new CLI flag, or new wiring in `chat.go`, needs a real-binary test in `cmd/durable`, because unit tests don't protect it.
- Tests that run the binary must use a temporary `HOME`.
- When a test fails, fix the code. Change the test's data only if the test is wrong.
- Real-binary tests share one binary, built by `TestMain`, so use `binPath` rather than building per test. CI builds it with `-race` through `GOFLAGS`.
- That `TestMain` runs its own `go build`, so mutation checks on them need `GOFLAGS=-overlay=<file>`, not `go test -overlay`.
