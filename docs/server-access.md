# Server access

How the browser reaches `durable-server` and signs in. This is phase 3's first slice, built and
validated before the UI; see [phases](architecture.md#phases).

The browser reaches the server at `https://durable.localhost:<port>`, with a certificate from
[mkcert](https://github.com/FiloSottile/mkcert), and signs in with a cookie that outlives restarts.

## Why TLS on loopback


- Browsers only speak HTTP/2 over TLS. On HTTP/1.1, Chrome allows about 6 connections per host, and
  each open `WatchEvents` holds one, so a few tabs starve unary calls.
- A name of its own gives WebAuthn a relying party ID that other local apps don't share. `localhost`
  is shared by every port, and an IP address can't be one.
- It allows `Secure` and `__Host-` cookies, and exercises the `https` origins that hosting needs.

With TLS configured, `serve` adds the configured host name on its port to the allowed hosts and
accepts `https` origins. That brings forward part of
[hosting](architecture.md#hosting-is-deferred). Without a certificate it serves plain HTTP with h2c
as now, and accepts bearer tokens only: cookie login needs TLS.

## Alternate host names

`alternate_hosts` in the config lists extra host names the UI is served on, for example a
Tailscale name such as `<machine>.<tailnet>.ts.net`. The server stays on loopback, and a forwarder
such as `tailscale serve` terminates TLS with its own certificate and proxies to it. Other devices
don't trust the mkcert CA, so the forwarder's certificate is the one they see.

- Each name is matched exactly, as a host and as an `https` origin. No wildcards or suffixes.
- Cookies are per host, so each name signs in separately.
- `oauth` needs a redirect address registered per name. `webauthn` binds a passkey to one relying
  party ID, and `durable.localhost` isn't a suffix of the Tailscale name, so a server with
  alternate names sets the relying party ID to one of them and passkeys work only there.
- A forwarder connects from loopback, so `serve` refuses to start with both `claim_first_user` and
  `alternate_hosts` set. Otherwise anyone on the tailnet could claim an empty server.
- Tailscale's identity headers are not a login method. Trusting them would add a second method.

## Files

All under `~/.durable`:

| Path | Holds | Lifetime |
|---|---|---|
| `server-config.json` | Host name, address, certificate paths, the login method and its settings | Written by `setup`, edited by hand |
| `certs/` | The mkcert certificate and key for the host name | Written by `setup`, reissued by it |
| `server.sqlite` | Users, their credentials, login links and browser sessions | Server only; no run ever writes it |
| `server.json` | Address and bearer token of the running server | Per run, as now |

`server.sqlite` doesn't break the [session index](architecture.md#session-index) rule against a
shared database: only the server's login code writes it, and session files stay separate.

## Commands

On `durable-server`:

- `setup` first checks that `mkcert` is on the `PATH` and its CA is installed (`mkcert -CAROOT`
  holds `rootCA.pem`). If not, it stops before writing anything and prints the fix, such as
  `brew install mkcert nss` then `mkcert -install`. It then writes the config, runs `mkcert` for the
  host name into `certs/`, and prints the URL, the certificate paths and, unless `claim_first_user`
  is set, a login link for the first user. It asks for the login method, recommending `oauth` or
  `webauthn`. It doesn't edit system files: if the name doesn't resolve to loopback, it prints the `/etc/hosts` line to add.
- `config` prints the same summary on demand: URL, address, certificate paths and expiry, the mkcert
  CA root, the login method, and whether the name resolves and the address accepts connections. It's
  for when you've forgotten.
- `login-link` creates a login link, for a new browser, a new user or an expired session.
- `serve`, the default, reads the config. Flags override it, so tests run without one.

The listen address covers what the name resolves to: macOS resolves `durable.localhost` to `::1`
first, so the server listens on both loopback addresses, or `config` reports the gap.

## One login method per server

`server-config.json` names exactly one browser login method:

| Method | Works offline | Identity is |
|---|---|---|
| `oauth` (recommended) | No | The exact issuer and subject from the provider's ID token |
| `webauthn` (recommended) | Yes | The credential ID, with relying party ID set to the host name |
| `password` | Yes | The username, with an argon2id hash and rate-limited attempts |

No mixing and no matching between identities:

- The server accepts only the configured method. There's no second method to fall back to, and no
  linking of accounts across methods.
- An identity matches only exactly. Email addresses are never used to match or allow a user,
  because providers don't guarantee they're stable or unique.
- With `oauth` and `claim_first_user` set in the config, the first sign-in on an empty
  `server.sqlite` saves its issuer and subject as the first user, with no link or allowlist. The
  claim closes once a user exists. `setup` sets it for `oauth` on a loopback address, and `serve`
  refuses to start with it set on any other address or with `alternate_hosts`, because a hosted or
  forwarded server would let anyone at the provider claim it first. Without it, later users, and
  the other methods, enroll through a login link. Later sign-ins must match exactly.
- Changing the method means enrolling every user again through login links. The old credentials
  are deleted, not converted.

## Login links and cookies

A login link is single use and expires after 15 minutes. It's stored hashed in `server.sqlite`, and
printed to stdout only, never to the log. Sign-in sets an `HttpOnly`, `Secure`, `SameSite=Strict`
`__Host-` session cookie backed by `server.sqlite`, so restarts don't log the browser out.

OAuth on loopback works where the provider accepts a `127.0.0.1` or `localhost` redirect. Hosting
changes the redirect address, and for `webauthn` the relying party ID, so users enroll again through
links.

Bearer tokens aren't a browser login method. They stay for the CLI and scripts, new on each run and
read from `server.json`. The auth check accepts the token or the configured method's cookie, and
Host and Origin checks apply to both.

## Users and attribution

Every signed-in user can approve shell commands on the host, so users are trusted equals with no
per-user permissions. The server sets `by` on an approval from the signed-in identity, and a
bearer-token caller is recorded as `token`. The client's `by` is ignored, and the comment on
`DecideApprovalRequest.by` changes in the same slice.

## Tests

Real-binary tests, as AGENTS.md requires for new commands and flags:

- `setup`, `config` and `login-link`.
- TLS, and cookie login for each method, with the other methods rejected.
- Single-use, expiring login links.
- `serve` refusing `claim_first_user` on a non-loopback address or with `alternate_hosts`.
- Exact matching of alternate host names.
