# Server access

How the browser reaches `durable-server` and authenticates.

## A stable token

One user, one machine, loopback only. The server keeps the bearer token it already requires, but
stops changing it on each run, so a restart no longer signs the browser out. This follows the
pattern of [Jupyter Server](https://jupyter-server.readthedocs.io/en/latest/operators/security.html)
and VS Code's `serve-web`: a random secret in a file only you can read, handed to the browser once
through a printed URL.

### What stays

- The server binds to loopback by default. `--addr` accepts any address, but `serve` logs a warning
  for a non-loopback one, because the API is then reachable over plain HTTP, guarded only by the
  token.
- The `Host` check, which stops DNS rebinding.
- The exact `Origin` check, scheme, host and port, with CORS off.
- `frame-ancestors 'none'` and `X-Frame-Options: DENY` on every response, so another site can't
  frame the UI and trick a click on Allow. Framing would pass the `Origin` check, because the framed
  page makes its own requests.
- A bearer token on every RPC. The `Host` and `Origin` checks already stop web pages, but the token
  also stops other users' processes on the machine, and anything outside a browser, from approving
  shell commands.

### The token

- `~/.durable/server-token` holds the token, mode `0600`. `serve` creates it on first start and
  reuses it after that.
- `server.json` still holds the running server's address and token, so the CLI and tests are
  unchanged.
- The token is never written to the log. `serve` logs its URL without the token, and says to run
  `durable-server url`.
- `serve` holds an exclusive lock on `~/.durable/server.lock` while it runs, so there's one server
  per home and only it writes and removes `server.json`. A second `serve` exits, pointing at
  `durable-server url`.

### Commands

On `durable-server`:

- `url` prints `http://127.0.0.1:<port>/#token=<token>` for the running server, read from
  `server.json`, or says no server is running. Like `jupyter server list`, it's for when you've
  forgotten.
- `rotate-token` writes a new token. It takes effect at the next start, and every browser needs the
  new URL.

### The browser

- The page reads the token from the URL fragment, which browsers never send to the server, so it
  stays out of request lines and `Referer` headers.
- It stores the token in `localStorage`, removes the fragment from the address bar, and sends it as
  `Authorization: Bearer`.
- On `Unauthenticated` it stops and asks for a new URL.

`localStorage` is per origin, so the browser uses the address `url` prints. A tab opened at
`localhost` instead of `127.0.0.1` needs the URL once more.

### Tradeoffs

- **The UI can read the token.** A script injected into the page could steal it. Transcripts carry
  model and tool output, which is untrusted, so the UI renders it as text or sanitized Markdown,
  never as raw HTML.
- **HTTP/1.1 only.** Without TLS, browsers allow about 6 connections per host, and each open
  `WatchEvents` holds one. That's fine for a session view per tab, and becomes a limit with many
  tabs.
- **Rotation is manual.** If the token leaks, run `rotate-token`, restart, and open the new URL.
