#!/usr/bin/env bash
# Build the deployable bundles:
#   dist/free-direct-dials-<version>-linux-amd64.tar.gz   (Intel/AMD VMs)
#   dist/free-direct-dials-<version>-linux-arm64.tar.gz   (ARM VMs, e.g. Oracle Ampere, AWS Graviton)
#   dist/SHA256SUMS
# Each bundle: the dials binary + deploy/ (install.sh, upgrade.sh, provision.sh,
# Caddyfile, dials.service, dials.env.example) + docs/ + VERSION + ARCH.
#
#   bash deploy/make-release.sh v1.0.0
set -euo pipefail
cd "$(dirname "$0")/.."
VER=${1:-$(git describe --tags --always --dirty)}
rm -rf dist && mkdir -p dist
for arch in amd64 arm64; do
  name="free-direct-dials-$VER-linux-$arch"
  root="dist/$name"
  mkdir -p "$root/deploy" "$root/docs"
  echo "== build $name"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VER" -o "$root/dials" ./cmd/dials
  cp deploy/install.sh deploy/upgrade.sh deploy/provision.sh deploy/Caddyfile deploy/dials.service deploy/dials.env.example "$root/deploy/"
  cp docs/DEPLOY.md docs/RUNBOOK.md docs/MONETIZATION.md "$root/docs/"
  cp README.md "$root/"
  echo "$VER" >"$root/VERSION"
  echo "$arch" >"$root/ARCH"
  opts=(--owner=0 --group=0 --numeric-owner)
  tar -C dist "${opts[@]}" --mode='a+rX,u+w,go-w' --exclude="$name/dials" -cf "dist/$name.tar" "$name"
  tar -C dist "${opts[@]}" --mode=0755 -rf "dist/$name.tar" "$name/dials" # the server stays executable
  gzip -9 -n "dist/$name.tar"
  rm -rf "$root"
done
(cd dist && for f in free-direct-dials-*.tar.gz; do printf '%s  %s
' "$(sha256sum "$f" | cut -d' ' -f1)" "$f"; done >SHA256SUMS)
echo
ls -l dist
cat dist/SHA256SUMS
