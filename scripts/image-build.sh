#!/usr/bin/env bash
# Build the hive-sandbox daemon image and record its digest.
#
#   ./scripts/image-build.sh
#
# Writes docker/hive-sandbox/digest.json. Same caveat as the harness and egress
# images: a locally built image has a machine-local digest, so this is build
# output rather than a committed pin. It becomes a real pin when this image
# gets a registry.
set -uo pipefail
cd "$(dirname "$0")/.."

export PODMAN_COMPOSE_WARNING_LOGS=false

containerfile="docker/hive-sandbox/Containerfile"
lockfile="docker/hive-sandbox/digest.json"
repo="localhost/hive-sandbox"
tag="${repo}:latest"

if ! command -v podman >/dev/null 2>&1; then
  echo "podman not found. The daemon image builds under rootless Podman (D7)." >&2
  exit 1
fi

version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

echo "==> building $tag (version $version)"
# Context is the repo root: the build stage compiles the whole module.
if ! podman build --target daemon --tag "$tag" --build-arg "VERSION=$version" -f "$containerfile" .; then
  echo "build failed" >&2
  exit 1
fi

digest=$(podman image inspect "$tag" --format '{{.Digest}}' 2>/dev/null)
if [ -z "$digest" ]; then
  echo "could not read a digest for $tag" >&2
  exit 1
fi

# The version the binary reports, read back out of the image rather than echoed
# from the variable above. HIVE_SANDBOX_VERSION reaches the binary only through the build stage. An image that reports
# "dev" while the lockfile claims a tag is the
# kind of thing that is discovered during an incident.
reported=$(podman run --rm "$tag" --version 2>/dev/null | sed "s/^hive-sandbox //" | tr -d "\r\n")
if [ "$reported" != "$version" ]; then
  echo "the image reports version '$reported' but was built as '$version'" >&2
  echo "that means HIVE_SANDBOX_VERSION did not reach the build; the pin would be a lie." >&2
  exit 1
fi

size=$(podman image inspect "$tag" --format '{{.Size}}' 2>/dev/null)

cat >"$lockfile" <<JSON
{
  "repository": "${repo}",
  "digest": "${digest}",
  "version": "${version}",
  "size_bytes": ${size:-0}
}
JSON

echo "    daemon  $digest  ($version, $((${size:-0} / 1024 / 1024)) MiB)"
echo
echo "DAEMON IMAGE BUILT"
echo "  pin recorded in $lockfile"
