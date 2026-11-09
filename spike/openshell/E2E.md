# Try durable-server with sandboxed tools (spike)

Each session gets its own OpenShell sandbox (`ds-<session id>`). The model loop, session database, hooks and
approvals stay on the host; `bash`, `edit_file`, `read_file`, `list_files` and `grep` run in the sandbox.

## Prerequisites

- Docker, and an OpenShell gateway that `openshell status` reports as Connected.
- Go and `mise install` done in this repository.
- A provider key. Keep it out of shell history and logs: use a 1Password reference or an exported variable.

## 1. Build and install

```sh
docker build -t durable-worker:spike -f spike/openshell/Dockerfile .
go build -o /tmp/durable-server ./cmd/durable-server
```

## 2. Serve with the sandbox

```sh
OPENROUTER_API_KEY=... /tmp/durable-server serve \
  --sandbox-image durable-worker:spike \
  --provider openrouter --model openai/gpt-5.6-luna \
  --idle-timeout 2m
```

Or read the key from 1Password: `--api-token-from-1password 'op://<vault>/<item>/<field>'`.
Provider flags are described in `docs/providers.md`.

## 3. Open the browser

```sh
/tmp/durable-server url
```

Open the printed link, create a session, and send:

> Run `uname -a; id` and tell me the output.

The call parks for approval. Approve it and watch the output stream live. Expect a Linux kernel line from the
sandbox (not Darwin) and `uid=1500(app)`, which proves the command ran in the sandbox and not on the host.

## 4. See the lifecycle

```sh
openshell sandbox list          # ds-<session id> is Ready while the session is open
```

After `--idle-timeout` with no browser tab on the session, the server stops the sandbox (phase Stopped; the
workspace is kept). Sending another message starts it again, so the first tool call after a stop is slower.
Ctrl-C on the server also stops every open session's sandbox.

## 5. Clean up

The server never deletes sandboxes.

```sh
/tmp/durable-server sandbox-delete <session id>   # or: openshell sandbox delete ds-<session id>
```

Known limits: the worker token is kept in server memory, so after a server restart an existing sandbox can't be
reached; delete it as above (this loses its workspace). The project's `AGENTS.md` and skills are still read from
the host directory the session was created in.
