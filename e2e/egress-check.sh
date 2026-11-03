#!/usr/bin/env bash
set -euo pipefail

fail() { echo "egress-check: FAIL: $1" >&2; exit 1; }
ok() { echo "egress-check: ok: $1"; }

if curl --noproxy '*' -sS --max-time 5 -o /dev/null https://example.com 2>/dev/null; then
  fail "direct egress to example.com succeeded"
fi
ok "direct egress blocked"

# Internal networks give no external DNS, so resolution must fail.
if getent hosts example.com >/dev/null 2>&1; then
  fail "external DNS resolves example.com"
fi
ok "external DNS blocked"

if curl -sS --max-time 10 -o /dev/null https://example.com 2>/dev/null; then
  fail "proxy allowed example.com"
fi
ok "proxy denies example.com"

for url in https://openrouter.ai/api/v1/models https://api.github.com/zen; do
  curl -fsS --max-time 20 -o /dev/null "$url" 2>/dev/null || fail "proxy blocked allowlisted $url"
  ok "proxy allows $url"
done

exec "$@"
