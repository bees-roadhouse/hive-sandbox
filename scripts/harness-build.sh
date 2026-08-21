#!/usr/bin/env bash
# Build the harness images and record the digests runs pin to.
#
#   ./scripts/harness-build.sh              # build all three
#   ./scripts/harness-build.sh claude       # build one, relock what exists
#
# Writes docker/harness/digests.json. That file is the pin (D12.5): a run
# records the digest it used, upgrading is a rebuild plus a changed digest, and
# rollback is putting the old digest back. Nothing runs a floating tag.
#
# The lockfile is derived from podman's actual image state rather than merged
# with its own previous contents, so it cannot drift from what is on disk.
set -uo pipefail
cd "$(dirname "$0")/.."

containerfile="docker/harness/Containerfile"
lockfile="docker/harness/digests.json"
repo="localhost/hive-sandbox-harness"
all_runtimes=(claude codex opencode)

if [ $# -gt 0 ]; then
  runtimes=("$@")
else
  runtimes=("${all_runtimes[@]}")
fi

if ! command -v podman >/dev/null 2>&1; then
  echo "podman not found. The harness builds and runs under rootless Podman (D7)." >&2
  exit 1
fi

for runtime in "${runtimes[@]}"; do
  case " ${all_runtimes[*]} " in
    *" $runtime "*) ;;
    *) echo "unknown runtime '$runtime'; expected one of: ${all_runtimes[*]}" >&2; exit 1 ;;
  esac
done

for runtime in "${runtimes[@]}"; do
  echo "==> building ${repo}:${runtime}"
  # --target selects the entrypoint stage. The base layer builds once and is
  # cached across all three.
  if ! podman build --target "$runtime" --tag "${repo}:${runtime}" -f "$containerfile" docker/harness; then
    echo "build failed for $runtime" >&2
    exit 1
  fi
done

echo "==> recording digests"
tmp=$(mktemp)
{
  echo "{"
  echo "  \"repository\": \"${repo}\","
  echo "  \"runtimes\": {"
  first=1
  for runtime in "${all_runtimes[@]}"; do
    tag="${repo}:${runtime}"
    digest=$(podman image inspect "$tag" --format '{{.Digest}}' 2>/dev/null)
    [ -z "$digest" ] && continue
    cli_version=$(podman image inspect "$tag" \
      --format '{{index .Labels "org.beesroadhouse.harness.cli-version"}}' 2>/dev/null)

    [ "$first" -eq 0 ] && echo ","
    first=0
    printf '    "%s": { "digest": "%s", "cli_version": "%s" }' \
      "$runtime" "$digest" "${cli_version:-unknown}"
    echo "    $runtime  $digest  (${cli_version:-unknown})" >&2
  done
  echo
  echo "  }"
  echo "}"
} >"$tmp"
mv "$tmp" "$lockfile"

echo
echo "HARNESS IMAGES BUILT"
echo "  pins recorded in $lockfile"
