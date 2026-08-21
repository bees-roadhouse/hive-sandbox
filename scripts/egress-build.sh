#!/usr/bin/env bash
# Build the egress proxy image and record its digest.
#
#   ./scripts/egress-build.sh
#
# Writes docker/egress/digest.json. Its own file rather than a block spliced
# into the harness lockfile: two independent build scripts editing one JSON file
# is a merge problem nobody asked for.
#
# Same caveat as the harness images: a locally built image has a machine-local
# digest, so this is build output rather than a committed pin.
set -uo pipefail
cd "$(dirname "$0")/.."

export PODMAN_COMPOSE_WARNING_LOGS=false

containerfile="docker/egress/Containerfile"
lockfile="docker/egress/digest.json"
repo="localhost/hive-sandbox-egress"
tag="${repo}:latest"

if ! command -v podman >/dev/null 2>&1; then
  echo "podman not found. The proxy builds and runs under rootless Podman (D7)." >&2
  exit 1
fi

version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

echo "==> building $tag (version $version)"
# Context is the repo root: the build stage compiles the whole module.
if ! podman build --target proxy --tag "$tag" --build-arg "VERSION=$version" -f "$containerfile" .; then
  echo "build failed" >&2
  exit 1
fi

digest=$(podman image inspect "$tag" --format '{{.Digest}}' 2>/dev/null)
if [ -z "$digest" ]; then
  echo "could not read a digest for $tag" >&2
  exit 1
fi

cat >"$lockfile" <<JSON
{
  "repository": "${repo}",
  "digest": "${digest}",
  "version": "${version}"
}
JSON

echo "    egress  $digest  ($version)"
echo
echo "EGRESS PROXY IMAGE BUILT"
echo "  pin recorded in $lockfile"
