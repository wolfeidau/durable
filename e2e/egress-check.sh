#!/usr/bin/env bash
set -euo pipefail

fail() { echo "egress-check: FAIL: $1" >&2; exit 1; }
ok() { echo "egress-check: ok: $1"; }

# An IP literal tests routing alone, since names already fail to resolve.
if curl --noproxy '*' -sS --max-time 5 -o /dev/null https://1.1.1.1 2>/dev/null; then
  fail "direct egress to 1.1.1.1 succeeded"
fi
ok "direct egress blocked"

# Internal networks give no external DNS, so resolution must fail.
if getent hosts example.com >/dev/null 2>&1; then
  fail "external DNS resolves example.com"
fi
ok "external DNS blocked"

# Require the proxy's 403, so an unreachable proxy can't pass as a denial.
status=$(curl -sS --max-time 10 -o /dev/null -w '%{http_connect}' https://example.com 2>/dev/null || true)
[ "$status" = 403 ] || fail "proxy did not deny example.com (CONNECT status $status)"
ok "proxy denies example.com"

# Check the proxy's CONNECT, not the site's answer, so an upstream rate limit can't pass as a block.
for url in https://openrouter.ai/api/v1/models https://api.github.com/zen; do
  status=$(curl -sS --max-time 20 -o /dev/null -w '%{http_connect}' "$url" 2>/dev/null || true)
  [ "$status" = 200 ] || fail "proxy blocked allowlisted $url (CONNECT status $status)"
  ok "proxy allows $url"
done

exec "$@"
