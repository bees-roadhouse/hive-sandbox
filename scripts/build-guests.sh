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
# job that rebuilds it and diffs the result can never be green. Three things
# make it so: the exact toolchain patch in rust-toolchain.toml; one guest
# workspace, so no symbol name carries this checkout's path; and RUSTFLAGS set
# HERE rather than inherited, because cargo mixes the flags into every symbol
# hash and a CI runner that exports `-D warnings` would build different names.
# The flags remap the two paths rustc embeds (panic locations name the file)
# to fixed names: the cargo registry and this checkout.
registry="${CARGO_HOME:-$HOME/.cargo}/registry/src"
export RUSTFLAGS="--remap-path-prefix=${registry}=/cargo/registry/src --remap-path-prefix=$(pwd)=/hive-sandbox"
unset CARGO_ENCODED_RUSTFLAGS

apps=("$@")
if [ ${#apps[@]} -eq 0 ]; then
  for dir in apps/*/; do
    [ -f "$dir/Cargo.toml" ] && apps+=("$(basename "$dir")")
  done
fi

for name in "${apps[@]}"; do
  [ -f "apps/$name/Cargo.toml" ] || { echo "no such app: apps/$name" >&2; exit 1; }
  echo "==> $name"
  # From the guest workspace root, so the build lands in guest/target.
  (cd guest && cargo build --release --target "$TARGET" -p "$name" --quiet)
  cp "guest/target/$TARGET/release/$name.wasm" "$OUT/$name.wasm"
  ls -l "$OUT/$name.wasm"
done
