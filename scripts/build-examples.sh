#!/usr/bin/env bash
#
# Builds the shipped examples into ./plugins/, which docker-compose mounts.
#
# A package directory must be named for the key its manifest declares — Core
# reports a mismatch as a packaging error — and the binary path must match
# runtime.entrypoint. Both are read from the manifest here rather than assumed.
#
# The target defaults to linux/amd64 because that is what the runtime image
# runs. Running Core straight off the development machine needs plugins for
# that machine instead — the kernel rejects the wrong format outright and Core
# reports `exec format error` for every plugin:
#
#   GOOS=darwin GOARCH=arm64 ./scripts/build-examples.sh
#
# A plugin with a frontend/ directory also gets its micro-frontend built and
# copied to <out>/<key>/frontend/, which is where Core looks for it. That part
# needs npm and takes far longer than the Go build, so it is skipped when npm
# is missing and can be turned off with SKIP_FRONTEND=1 — changing Go code does
# not need the pages rebuilt.
set -euo pipefail

cd "$(dirname "$0")/.."
out=${1:-plugins}
goos=${GOOS:-linux}
goarch=${GOARCH:-amd64}
skip_frontend=${SKIP_FRONTEND:-}

if [ -z "$skip_frontend" ] && ! command -v npm >/dev/null 2>&1; then
  echo "note: npm not found, building backends only (plugin pages will be missing)"
  skip_frontend=1
fi

echo "target: $goos/$goarch -> $out"

for src in extension-example/*/; do
  name=$(basename "$src")
  [ -f "$src/manifest.yaml" ] || continue

  key=$(grep -E '^key:' "$src/manifest.yaml" | head -1 | awk '{print $2}')
  entrypoint=$(grep -E '^\s+entrypoint:' "$src/manifest.yaml" | head -1 | awk '{print $2}')

  echo "building $name -> $out/$key/$entrypoint"
  mkdir -p "$out/$key/$(dirname "$entrypoint")"
  # Static: the runtime image is musl-based and a dynamically linked binary
  # fails to exec there with a message that does not say why.
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -o "$out/$key/$entrypoint" "./$src"
  cp "$src/manifest.yaml" "$out/$key/manifest.yaml"

  # Core decides a plugin has a UI by the presence of <package>/frontend, so an
  # empty directory would put a menu entry in front of a page that 404s. Only
  # copy one when the build actually produced something.
  [ -n "$skip_frontend" ] && continue
  [ -f "$src/frontend/package.json" ] || continue

  echo "  frontend: $name"
  ( cd "$src/frontend"
    if [ -f package-lock.json ]; then npm ci --silent --no-audit --no-fund
    else npm install --silent --no-audit --no-fund; fi
    npm run build --silent )

  if [ -f "$src/frontend/dist/index.html" ]; then
    rm -rf "${out:?}/$key/frontend"
    mkdir -p "$out/$key/frontend"
    cp -R "$src/frontend/dist/." "$out/$key/frontend/"
  else
    echo "  warning: $name built no dist/index.html; leaving it backend-only"
  fi
done

echo "done: $(ls "$out")"
