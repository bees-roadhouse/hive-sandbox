#!/usr/bin/env bash
# Stop the local development Garage.
#
#   ./scripts/garage-down.sh           # stop and remove the container, keep the data
#   ./scripts/garage-down.sh --purge   # also delete the volume, so garage-up starts empty
set -uo pipefail
cd "$(dirname "$0")/.."

export PODMAN_COMPOSE_WARNING_LOGS=false

compose_file="docker/docker-compose.garage.yml"

if [ -n "${HIVE_SANDBOX_COMPOSE:-}" ]; then
  # shellcheck disable=SC2206 # word splitting is what we want here
  compose_cmd=($HIVE_SANDBOX_COMPOSE)
elif command -v podman >/dev/null 2>&1; then
  compose_cmd=(podman compose)
elif command -v docker >/dev/null 2>&1; then
  compose_cmd=(docker compose)
elif command -v podman-compose >/dev/null 2>&1; then
  compose_cmd=(podman-compose)
else
  echo "no compose provider found. Install Podman 5+, or Docker, or set HIVE_SANDBOX_COMPOSE." >&2
  exit 1
fi
compose_cmd+=(-f "$compose_file")

args=(down)
if [ "${1:-}" = "--purge" ]; then
  args+=(-v)
fi

echo "==> compose ${args[*]}"
"${compose_cmd[@]}" "${args[@]}"
if [ $? -ne 0 ]; then
  echo "compose down failed" >&2
  exit 1
fi

echo
if [ "${1:-}" = "--purge" ]; then
  echo "GARAGE DOWN, volumes deleted"
else
  echo "GARAGE DOWN, volumes hive-sandbox-garage-meta and -data kept (--purge deletes them)"
fi
