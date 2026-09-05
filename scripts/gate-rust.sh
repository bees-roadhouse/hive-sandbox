#!/usr/bin/env bash
# The born-green gate. Run before pushing; read the OUTPUT, never the exit code
# of a piped command.
#
#   export HIVE_SANDBOX_TEST_DATABASE_URL="$(./scripts/db-up.sh --quiet)"
#   ./scripts/gate-rust.sh
#
# fmt, clippy, build, test, and a NAMED list of every test that skipped, because
# a skip is a test saying it is not answering the question and that only helps
# if somebody hears it.
#
# The database line is not optional and the gate refuses without it. It used to
# be a suggestion, and the result was every Postgres-backed test in the repo
# skipping itself while the gate printed GATE GREEN in about the same wall time.
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

# The browser client is a committed build (web/dist) embedded into the daemon.
# When node is present the gate rebuilds it and refuses a diff, so a change to
# web/ that nobody rebuilt cannot ship stale bytes. Without node the committed
# build is what gets embedded, and CI has node.
if command -v npm >/dev/null 2>&1; then
  echo "==> web"
  if (cd web && npm ci --no-fund --no-audit --silent && npm run --silent typecheck && npm run --silent build); then
    if ! git diff --quiet -- web/dist; then
      echo "web/dist is out of date with web/src; commit the rebuilt files:" >&2
      git --no-pager diff --stat -- web/dist >&2
      failed+=("web-dist")
    fi
  else
    failed+=("web")
  fi
else
  echo "==> web (npm not found; embedding the committed web/dist as is)"
fi

step fmt cargo fmt --all -- --check
step clippy cargo clippy --workspace --all-targets -- -D warnings
step build cargo build --workspace --all-targets

echo "==> test"
log=$(mktemp)
# --nocapture so a SKIPPED: line reaches this script; the test harness would
# otherwise swallow it along with everything else a passing test printed.
# --no-fail-fast so one red crate does not hide the others' results: the point
# of a gate is the whole picture, not the first thing that broke.
cargo test --workspace --no-fail-fast -- --nocapture 2>&1 | tee "$log"
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
