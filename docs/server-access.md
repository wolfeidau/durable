# Server access

How the browser reaches `durable-server` and signs in. This is phase 3's first slice, built and
validated before the UI; see [phases](architecture.md#phases).

The browser reaches the server at `https://localhost:<port>`, with a certificate from
[mkcert](https://github.com/FiloSottile/mkcert), and signs in with a passkey. A cookie keeps it
signed in across restarts.

## First cut

One user, one machine, loopback only.

### TLS on localhost

- Browsers only speak HTTP/2 over TLS. On HTTP/1.1, Chrome allows about 6 connections per host, and
  each open `WatchEvents` holds one, so a few tabs starve unary calls.
- It allows `Secure` and `__Host-` cookies.

The allowed hosts stay as now, `localhost` and `127.0.0.1` on the server's port. With TLS
configured, `serve` accepts `https` origins instead of `http`. Without a certificate it serves plain
HTTP with h2c as now, and accepts bearer tokens only: cookie login needs TLS.

macOS resolves `localhost` to `::1` first, so the server listens on both loopback addresses.

### Plain `localhost` is a compromise

Browsers scope cookies and WebAuthn to the host name, not the port, so every app served on
`localhost` shares them:

- Another local server the browser visits receives durable's session cookie in its own requests,
  and could replay it.
- Passkeys for durable sit under relying party ID `localhost`, next to any other local app's.

This is accepted for the first cut. Any process running as you can already read the bearer token
in `server.json`, so local processes are trusted either way. What the server still enforces:

- A cookie-authenticated request must carry an `Origin` header that matches exactly, scheme, host
  and port. A browser page on another port can't use the cookie.
- A WebAuthn assertion must carry the exact origin, port included, in its client data.

A host name of its own, such as `durable.localhost`, would isolate both. It's shelved because an
OAuth provider may not accept it as a redirect address; see [later](#later).

### Files

All under `~/.durable`:

| Path | Holds | Lifetime |
|---|---|---|
| `server-config.json` | Address and certificate paths | Written by `setup`, edited by hand |
| `certs/` | The mkcert certificate and key for `localhost`, `127.0.0.1` and `::1` | Written by `setup`, reissued by it |
| `server.sqlite` | Passkeys, enrollment links and browser sessions | Server only; no run ever writes it |
| `server.json` | Address and bearer token of the running server | Per run, as now |

`server.sqlite` doesn't break the [session index](architecture.md#session-index) rule against a
shared database: only the server's login code writes it, and session files stay separate.

### Commands

On `durable-server`:

- `setup` first checks that `mkcert` is on the `PATH` and its CA is installed (`mkcert -CAROOT`
  holds `rootCA.pem`). If not, it stops before writing anything and prints the fix, such as
  `brew install mkcert nss` then `mkcert -install`. It then writes the config, runs `mkcert` into
  `certs/`, and prints the URL, the certificate paths and an enrollment link.
- `config` prints the same summary on demand: URL, address, certificate paths and expiry, the mkcert
  CA root, and whether the address accepts connections. It's for when you've forgotten.
- `enroll-link` creates an enrollment link, to add a passkey on a new browser or device.
- `revoke` ends every browser session, so each browser signs in again with its passkey.
- `serve`, the default, reads the config. Flags override it, so tests run without one.

### Passkeys

Sign-in is WebAuthn only, with no password, through
[go-webauthn](https://github.com/go-webauthn/webauthn) and relying party ID `localhost`. Touch ID or
a security key on a Mac.

- An enrollment link registers one passkey. It's single use, expires after 15 minutes, is stored
  hashed in `server.sqlite`, and is printed to stdout only, never to the log.
- A passkey is identified by its credential ID, matched exactly.
- Sign-in sets an `HttpOnly`, `Secure`, `SameSite=Strict` `__Host-` session cookie backed by
  `server.sqlite`. It lasts 30 days and renews while in use, so restarts don't sign the browser out.

Bearer tokens aren't a browser login method. They stay for the CLI and scripts, new on each run and
read from `server.json`. The auth check accepts the token or a session cookie. Host checks apply to
both, and cookie requests also need a matching `Origin`.

With one user, `by` on an approval stays free text.

### Tests

Real-binary tests, as AGENTS.md requires for new commands and flags:

- `setup` and `config`, including `setup` stopping when `mkcert` or its CA is missing.
- `enroll-link`: single use, expiry, and never written to the log.
- Serving over TLS, with `https` origins accepted.
- A session cookie that still works after a restart, and stops working after `revoke`.
- A cookie request with a missing or wrong `Origin` rejected.

WebAuthn ceremonies use a software authenticator in the test. The browser check reruns the spike
page signed in by passkey, over HTTP/2, and restarts the server.

## Later

Shelved until a second user, a second device or hosting needs them. The rules below hold when they
arrive.

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
