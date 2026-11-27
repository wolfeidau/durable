#!/bin/sh
# Installs the durable and durable-server release binaries for this OS and architecture.
# Usage: install.sh [--version vX.Y.Z] [--prefix DIR]
set -eu

repo="wolfeidau/durable"
# The release pipeline's Buildkite OIDC identity; only its tag builds sign provenance.
signer="https://buildkite.com/wolfeidauci/durable-release"
version="latest"
prefix="${HOME}/.local"

while [ $# -gt 0 ]; do
  case "$1" in
    --version)
      [ $# -ge 2 ] || { echo "--version requires a value" >&2; exit 1; }
      version="$2"
      shift 2
      ;;
    --prefix)
      [ $# -ge 2 ] || { echo "--prefix requires a value" >&2; exit 1; }
      prefix="$2"
      shift 2
      ;;
    -h | --help)
      echo "usage: install.sh [--version vX.Y.Z] [--prefix DIR]"
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) target="darwin_arm64" ;;
  Linux-x86_64) target="linux_amd64" ;;
  Linux-aarch64 | Linux-arm64) target="linux_arm64" ;;
  *) echo "unsupported platform: $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

asset="durable_${target}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

bundle="${tmp}/${asset}.sigstore.json"
use_gh=0
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  use_gh=1
fi

if [ "${use_gh}" -eq 1 ]; then
  if [ "${version}" = "latest" ]; then
    tag="$(gh release view --repo "${repo}" --json tagName --jq .tagName)"
  else
    tag="${version}"
  fi
  gh release download "${tag}" --repo "${repo}" --dir "${tmp}" \
    --pattern "${asset}" --pattern "${asset}.sha256" --pattern "${asset}.sigstore.json"
else
  if [ "${version}" = "latest" ]; then
    base="https://github.com/${repo}/releases/latest/download"
  else
    base="https://github.com/${repo}/releases/download/${version}"
  fi
  curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}"
  curl -fsSL -o "${tmp}/${asset}.sha256" "${base}/${asset}.sha256"
  curl -fsSL -o "${bundle}" "${base}/${asset}.sigstore.json"
fi

expected="$(cut -d' ' -f1 <"${tmp}/${asset}.sha256")"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${tmp}/${asset}" | cut -d' ' -f1)"
else
  actual="$(shasum -a 256 "${tmp}/${asset}" | cut -d' ' -f1)"
fi
if [ "${expected}" != "${actual}" ]; then
  echo "checksum mismatch for ${asset}: expected ${expected}, got ${actual}" >&2
  exit 1
fi
verified="checksum"

# GitHub signs immutable releases itself, so this proves the archive was published in that release.
if [ "${use_gh}" -eq 1 ] && gh release verify-asset --help >/dev/null 2>&1 &&
  [ "$(gh release view "${tag}" --repo "${repo}" --json isImmutable --jq .isImmutable)" = "true" ]; then
  if ! gh release verify-asset "${tag}" "${tmp}/${asset}" --repo "${repo}" >/dev/null; then
    echo "release attestation check failed for ${asset}" >&2
    exit 1
  fi
  verified="${verified}, release attestation"
fi

# SLSA v1 provenance signed keyless by the tag-only release pipeline via Buildkite OIDC.
if command -v cosign >/dev/null 2>&1; then
  if [ ! -f "${bundle}" ] || ! cosign verify-blob-attestation --bundle "${bundle}" --type slsaprovenance1 \
    --certificate-oidc-issuer https://agent.buildkite.com --certificate-identity "${signer}" \
    "${tmp}/${asset}" >/dev/null 2>&1; then
    echo "build provenance check failed for ${asset} (expected signer ${signer})" >&2
    exit 1
  fi
  verified="${verified}, build provenance"
fi
echo "verified: ${verified}" >&2

tar -xzf "${tmp}/${asset}" -C "${tmp}" durable durable-server
mkdir -p "${prefix}/bin"
install -m 0755 "${tmp}/durable" "${prefix}/bin/durable"
install -m 0755 "${tmp}/durable-server" "${prefix}/bin/durable-server"
echo "installed durable $("${prefix}/bin/durable" --version) and durable-server to ${prefix}/bin"

case ":${PATH}:" in
  *":${prefix}/bin:"*) ;;
  *) echo "add ${prefix}/bin to your PATH" >&2 ;;
esac
