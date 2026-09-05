#!/usr/bin/env bash
# Build the first-party guest apps and drop them where the host tests read them.
#
#   ./scripts/build-guests.sh            # every app under apps/*/
#   ./scripts/build-guests.sh hello      # one
#
# The profile in each app's Cargo.toml is not incidental. Read
# scripts/guest-build.md before changing a setting.
set -euo pipefail
cd "$(dirname "$0")/.."

TARGET=${TARGET:-wasm32-wasip1}
OUT=${OUT:-crates/hive-wasmhost/testdata}

if ! command -v cargo >/dev/null 2>&1 && [ -x "$HOME/.cargo/bin/cargo" ]; then
  export PATH="$HOME/.cargo/bin:$PATH"
fi
if ! command -v cargo >/dev/null 2>&1; then
  echo "cargo not found. rustup.rs installs it; rust-toolchain.toml pins the version." >&2
  exit 1
fi
if ! rustup target list --installed 2>/dev/null | grep -qx "$TARGET"; then
  echo "==> adding the $TARGET target"
  rustup target add "$TARGET"
fi

mkdir -p "$OUT"

# The committed .wasm has to be byte-identical wherever it is built, or the CI
# job that rebuilds it and diffs the result can never be green. rustc embeds
# source paths (panic locations name the file), so the two places a path
# differs between machines are remapped to fixed names: the cargo registry and
# this checkout. The toolchain hash under /rustc/ is already fixed by the pin.
registry="${CARGO_HOME:-$HOME/.cargo}/registry/src"
export RUSTFLAGS="${RUSTFLAGS:-} --remap-path-prefix=${registry}=/cargo/registry/src --remap-path-prefix=$(pwd)=/hive-sandbox"

apps=("$@")
if [ ${#apps[@]} -eq 0 ]; then
  for dir in apps/*/; do
    [ -f "$dir/Cargo.toml" ] && apps+=("$(basename "$dir")")
  done
fi

for name in "${apps[@]}"; do
  dir="apps/$name"
  [ -f "$dir/Cargo.toml" ] || { echo "no such app: $dir" >&2; exit 1; }
  echo "==> $name"
  (cd "$dir" && cargo build --release --target "$TARGET" --quiet)
  cp "$dir/target/$TARGET/release/$name.wasm" "$OUT/$name.wasm"
  ls -l "$OUT/$name.wasm"
done
