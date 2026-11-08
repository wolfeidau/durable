# Server access

How the browser reaches `durable-server` and authenticates. This is phase 3's first slice, built and
validated before the UI; see [phases](architecture.md#phases).

## First cut: a stable token

**Built in #69.**

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

### Tests

Real-binary tests, as AGENTS.md requires for new commands:

- The token is the same across two runs, and the file is mode `0600`.
- `rotate-token` changes it, and the old token is rejected after a restart.
- `url` prints the running server's address and token, and reports when none is running.
- The server's log never contains the token.
- A second `serve` on the same home is refused, and the first keeps its `server.json`. A restart
  after `kill -9` succeeds, because the kernel drops the lock.

The browser check reruns the spike page, opened from `url`, and restarts the server: the page keeps
streaming without a new URL.

## Later

Shelved until Tailscale or hosting makes the server reachable from another machine. The decisions
below hold when they arrive.

### Content Security Policy

Deferred. A full policy, `default-src 'self'; script-src 'self'; connect-src 'self'; object-src
'none'; base-uri 'none'`, plus `X-Content-Type-Options: nosniff`, is defense in depth: the UI can
read the token from `localStorage`, and injected script could steal it.

- The inline `<style>` in `web/index.html` must move to a file first, or the policy needs
  `style-src 'self' 'unsafe-inline'`.
- Wait until the session view settles the page's assets.
- `frame-ancestors 'none'` already ships in the same header, so the full policy replaces the one
  `denyFraming` sets and must keep that directive.

### TLS

Serve at `https://localhost:<port>`, or a forwarder's host name, with a certificate from
[mkcert](https://github.com/FiloSottile/mkcert) for local use.

- HTTP/2, so streams no longer count against the per-host connection limit.
- `durable-server setup` checks that `mkcert` is on the `PATH` and its CA is installed, stopping
  before writing anything and printing the fix if not. It writes `server-config.json` and
  `certs/`. A `config` command prints the URL, certificate paths and expiry, and the mkcert CA root.
- With TLS, `serve` accepts `https` origins.

### Browser sign-in

A session cookie, `HttpOnly`, `Secure`, `SameSite=Strict` and `__Host-`, backed by a server-only
`server.sqlite`, replaces the token in the browser. Requirements found in review:

- Passkeys (WebAuthn) are the preferred method, so sign-in is passwordless. Enrollment links are
  single use, expire after 15 minutes, carry their secret in the URL fragment, are stored hashed,
  and are printed to stdout only.
- A cookie request needs an `Origin` that matches exactly. A WebAuthn assertion must carry the exact
  origin, port included.
- Open `WatchEvents` streams check their session again at each heartbeat, and end with
  `Unauthenticated` once it's revoked or expired.
- `serve` checks sessions against the database on every request, so a `revoke` from another process
  takes effect at once.
- On plain `localhost`, cookies and passkeys are shared with every other local app, because browsers
  scope both by host, not port. A host name of its own isolates them, but has to be one an OAuth
  provider accepts as a redirect address.

### One login method per server

`server-config.json` names exactly one browser login method, `webauthn` or `oauth`, and the server
accepts only that one. There's no fallback and no linking of accounts across methods. Changing the
method means enrolling every user again; the old credentials are deleted, not converted.

- **OAuth or OIDC.** The identity is the exact issuer and subject from the ID token, so plain OAuth
  without an ID token, such as GitHub's, doesn't qualify. The redirect address must be the host the
  UI is served on, or the cookie lands on the wrong host. Check that the provider accepts that
  address first: Google requires a public suffix and exempts only "localhost URIs" from HTTPS
  ([rules](https://developers.google.com/identity/protocols/oauth2/web-server#uri-validation)). The
  client secret's storage needs deciding.
- **First user claim.** With `oauth` and `claim_first_user` in the config, the first sign-in on an
  empty `server.sqlite` becomes the first user, with no link. `serve` refuses to start with it on a
  non-loopback address or with alternate host names, because then anyone at the provider could
  claim the server first.
- **Exact matching.** Email addresses are never used to match or allow a user, because providers
  don't guarantee they're stable or unique.
- **Password.** Only if a passkey isn't possible: an argon2id hash and rate-limited attempts.

### More users

Every signed-in user can approve shell commands on the host, so users are trusted equals with no
per-user permissions. The server sets `by` on an approval from the signed-in identity, and a
bearer-token caller is recorded as `token`. The client's `by` is ignored, and the comment on
`DecideApprovalRequest.by` changes in the same slice.

### Alternate host names

`alternate_hosts` lists extra host names the UI is served on, for example a Tailscale name such as
`<machine>.<tailnet>.ts.net` behind `tailscale serve`, which terminates TLS with its own
certificate.

- Each name is matched exactly, as a host and as an `https` origin. No wildcards or suffixes.
- Cookies are per host, so each name signs in separately, and a passkey works only on the name its
  relying party ID matches.
- A forwarder connects from loopback, so a loopback check alone doesn't prove a local user.
- Tailscale's identity headers are not a login method. Trusting them would add a second method.
- Check first whether `tailscale serve` passes the original `Host` through.
