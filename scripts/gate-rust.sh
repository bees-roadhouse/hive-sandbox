#!/usr/bin/env bash
# The born-green gate for the Rust tree. Mirrors scripts/gate.sh: fmt, lint,
# build, test, and a NAMED list of every test that skipped, because a skip is
# a test saying it is not answering the question and that only helps if
# somebody hears it.
#
#   export HIVE_SANDBOX_TEST_DATABASE_URL="$(./scripts/db-up.sh --quiet)"
#   ./scripts/gate-rust.sh
#
# The database line is not optional and the gate refuses without it, for the
# reason the Go gate gives: without it every integration test skips itself and
# a green gate reports nothing.
set -uo pipefail
cd "$(dirname "$0")/.."

# rustup installs to ~/.cargo/bin and a fresh shell does not always have it.
if ! command -v cargo >/dev/null 2>&1 && [ -x "$HOME/.cargo/bin/cargo" ]; then
  export PATH="$HOME/.cargo/bin:$PATH"
fi
if ! command -v cargo >/dev/null 2>&1; then
  echo "cargo not found. rustup.rs installs it; rust-toolchain.toml pins the version." >&2
  exit 1
fi
if [ -z "${HIVE_SANDBOX_TEST_DATABASE_URL:-}" ]; then
  echo "refusing to run: HIVE_SANDBOX_TEST_DATABASE_URL is unset." >&2
  echo "  export HIVE_SANDBOX_TEST_DATABASE_URL=\"\$(./scripts/db-up.sh --quiet)\"" >&2
  exit 1
fi

failed=()
step() {
  local name=$1; shift
  echo "==> $name"
  "$@" || failed+=("$name")
}

step fmt cargo fmt --all -- --check
step clippy cargo clippy --workspace --all-targets -- -D warnings
step build cargo build --workspace --all-targets

echo "==> test"
log=$(mktemp)
# --nocapture so a SKIPPED: line reaches this script; the test harness would
# otherwise swallow it along with everything else a passing test printed.
cargo test --workspace -- --nocapture 2>&1 | tee "$log"
test_status=${PIPESTATUS[0]}
[ "$test_status" -eq 0 ] || failed+=("test")

skipped=$(grep -E '^SKIPPED: ' "$log" | sed -E 's/^SKIPPED: ([^ ]+).*/  \1/' | sort -u)
rm -f "$log"
if [ -n "$skipped" ]; then
  echo
  echo "SKIPPED ($(echo "$skipped" | wc -l)) ... these did NOT run:"
  echo "$skipped"
fi

echo
if [ ${#failed[@]} -ne 0 ]; then
  echo "GATE RED: ${failed[*]}"
  exit 1
fi
echo "GATE GREEN"
