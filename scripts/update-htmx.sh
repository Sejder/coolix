#!/usr/bin/env bash
# Vendor the latest htmx release into internal/web/static/vendor, verified
# against the npm registry's integrity hash. Usage: scripts/update-htmx.sh
set -euo pipefail
cd "$(dirname "$0")/.."
meta=$(curl -sf https://registry.npmjs.org/htmx.org/latest)
version=$(jq -r .version <<<"$meta")
integrity=$(jq -r .dist.integrity <<<"$meta")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -sfL "$(jq -r .dist.tarball <<<"$meta")" -o "$tmp/htmx.tgz"
got="sha512-$(openssl dgst -sha512 -binary "$tmp/htmx.tgz" | base64 -w0)"
[[ "$got" == "$integrity" ]] || { echo "integrity mismatch: $got != $integrity" >&2; exit 1; }
tar -xzf "$tmp/htmx.tgz" -C "$tmp"
dst=internal/web/static/vendor
{
  echo "/* htmx $version (BSD-2-Clause), https://htmx.org. Vendored unmodified from npm htmx.org@$version, integrity $integrity (tarball). Update with scripts/update-htmx.sh. */"
  cat "$tmp/package/dist/htmx.min.js"
} > "$dst/htmx.min.js"
cp "$tmp/package/LICENSE" "$dst/htmx.LICENSE"
echo "htmx $version"
