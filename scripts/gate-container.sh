#!/usr/bin/env bash
# Run the born-green gate inside a container.
#
#   ./scripts/gate-container.sh            # build the toolchain image if needed, run the gate
#   ./scripts/gate-container.sh --rebuild  # rebuild the toolchain image first
#   ./scripts/gate-container.sh -- go test ./internal/store -run TestX -race -v
#
# Everything after `--` runs in place of the gate, so a single test is one
# command on a machine with no Go toolchain.
#
# This runs the ORDINARY scripts/gate.sh inside a container rather than
# reimplementing it. There is one gate; this only supplies the toolchain. A
# containerized variant with its own step list would drift from the real one and
# the drift would be invisible until CI disagreed with a laptop.
set -uo pipefail
cd "$(dirname "$0")/.."

image="localhost/hive-sandbox-gate:latest"
containerfile="docker/gate/Containerfile"
cache=".gocache"

rebuild=0
if [ "${1:-}" = "--rebuild" ]; then
  rebuild=1
  shift
fi

cmd=(./scripts/gate.sh)
if [ "${1:-}" = "--" ]; then
  shift
  if [ $# -eq 0 ]; then
    echo "nothing after --; pass a command to run instead of the gate" >&2
    exit 1
  fi
  cmd=("$@")
fi

if ! command -v podman >/dev/null 2>&1; then
  echo "podman not found. That is the only thing this needs; the Go toolchain," >&2
  echo "golangci-lint and the C compiler for -race all live in the image." >&2
  exit 1
fi

if [ "$rebuild" -eq 1 ] || ! podman image exists "$image"; then
  echo "==> building $image (once; cached after this)"
  if ! podman build --target gate --tag "$image" -f "$containerfile" .; then
    echo "toolchain image build failed" >&2
    exit 1
  fi
fi

# A database is as mandatory here as it is on the host: scripts/gate.sh refuses
# without one, and the whole point of that refusal is that it cannot be worked
# around by changing how the gate is invoked.
url="${HIVE_SANDBOX_TEST_DATABASE_URL:-}"
if [ -z "$url" ]; then
  echo "==> no HIVE_SANDBOX_TEST_DATABASE_URL; bringing the database up"
  url=$(./scripts/db-up.sh --quiet)
  if [ -z "$url" ]; then
    echo "could not start a database. Run ./scripts/db-up.sh and read its output." >&2
    exit 1
  fi
fi

# --network=host so 127.0.0.1:55432 means the same thing inside the container as
# outside it, and the connection string needs no rewriting. The dev database
# binds to loopback on purpose, so a bridge network would need
# host.containers.internal and a second spelling of the same URL ... one more
# thing that can be right in one place and wrong in the other.
#
# --security-opt label=disable rather than a :z/:Z mount: SELinux is enforcing on
# the maintainer's box, and relabelling somebody's git checkout as a side effect
# of running the tests is not a thing a test script should do.
mkdir -p "$cache/build" "$cache/mod"

echo "==> gate in $image"
podman run --rm \
  --network=host \
  --userns=keep-id \
  --security-opt label=disable \
  -v "$PWD:/src" \
  -v "$PWD/$cache/build:/gocache/build" \
  -v "$PWD/$cache/mod:/gocache/mod" \
  -e HIVE_SANDBOX_TEST_DATABASE_URL="$url" \
  -e GOCACHE=/gocache/build \
  -e GOMODCACHE=/gocache/mod \
  -e HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS="${HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS:-}" \
  -w /src \
  "$image" \
  "${cmd[@]}"
status=$?

# The gate's own contract: read the OUTPUT, not an exit code that survived a
# pipe. This one has not been through a pipe, so it is worth passing on intact.
exit $status
