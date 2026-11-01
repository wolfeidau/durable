## Public repository

This repository is public. Keep secrets and private details out of commits, code, tests, issues and pull requests, including API keys, 1Password references (vault, item or field names), account details and private paths. Use placeholders such as `op://<vault>/<item>/<field>`.

When running something that needs a credential, keep it out of command output and logs: pass it to the one command that needs it, and never print, write or commit it. Tool errors can echo a secret reference, so silence or redact their stderr.

## Verification

Fast loop, skipping the real-binary tests in `cmd/durable`, which take about 30s:

```sh
go test . ./claude ./internal/... ./tools/...
```

Before finishing, after your last edit:

```sh
go vet ./... && go test -race -count=1 ./...
mise exec -- golangci-lint run ./...
mise run check:tidy
```

## Durability invariants

- A state change and its events commit in the same transaction.
- `migrate` only runs `CREATE TABLE IF NOT EXISTS`, so a new table reaches existing sessions but a new or changed column doesn't.
- The database pool has one connection, so close a cursor before running another query or calling a callback.
- Wrappers around a tool must forward the optional interfaces `Recoverer` and `ReadOnlyTool`, because embedding `Tool` doesn't promote them.
- The system prompt and tool order must not change within a session. They feed the prompt cache and stored thinking blocks.
- MCP tools are pinned per session, and server hints never change replay or approval, because a hint is only the server's claim.
- The skill catalog is pinned per session, for the same reason as the tool order: it feeds the `skill` tool's description and so the prompt prefix.

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
- Real-binary tests run their own `go build`, so mutation checks on them need `GOFLAGS=-overlay=<file>`, not `go test -overlay`.
