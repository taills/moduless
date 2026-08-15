#!/usr/bin/env bash
#
# Builds the shipped examples into ./plugins/, which docker-compose mounts.
#
# A package directory must be named for the key its manifest declares — Core
# reports a mismatch as a packaging error — and the binary path must match
# runtime.entrypoint. Both are read from the manifest here rather than assumed.
set -euo pipefail

cd "$(dirname "$0")/.."
out=${1:-plugins}

for src in extension-example/*/; do
  name=$(basename "$src")
  [ -f "$src/manifest.yaml" ] || continue

  key=$(grep -E '^key:' "$src/manifest.yaml" | head -1 | awk '{print $2}')
  entrypoint=$(grep -E '^\s+entrypoint:' "$src/manifest.yaml" | head -1 | awk '{print $2}')

  echo "building $name -> $out/$key/$entrypoint"
  mkdir -p "$out/$key/$(dirname "$entrypoint")"
  # Static, Linux: the runtime image is musl-based and a dynamically linked
  # binary fails to exec there with a message that does not say why.
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$out/$key/$entrypoint" "./$src"
  cp "$src/manifest.yaml" "$out/$key/manifest.yaml"
done

echo "done: $(ls "$out")"
