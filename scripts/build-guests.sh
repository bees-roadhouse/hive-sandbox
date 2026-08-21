#!/usr/bin/env bash
# Build the first-party guest apps and drop them where the host tests read them.
#
# The flags are not incidental. Read scripts/guest-build.md before changing one.
set -euo pipefail
cd "$(dirname "$0")/.."

TARGET=${TARGET:-wasip1}
OUT=${OUT:-internal/wasmhost/testdata}

if ! command -v tinygo >/dev/null 2>&1; then
  echo "tinygo not found. Install it from https://github.com/tinygo-org/tinygo/releases"
  echo "and make sure binaryen's wasm-opt is on PATH too (tinygo shells out to it)."
  exit 1
fi

mkdir -p "$OUT"

for app in apps/*/; do
  name=$(basename "$app")
  [ -f "$app/go.mod" ] || continue
  echo "==> $name"
  (
    cd "$app"
    tinygo build \
      -target="$TARGET" \
      -buildmode=c-shared \
      -scheduler=none \
      -o "../../$OUT/$name.wasm" \
      ./
  )
  ls -l "$OUT/$name.wasm"
done
