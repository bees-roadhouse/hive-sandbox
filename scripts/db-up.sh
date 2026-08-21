#!/usr/bin/env bash
# Bring up the local development Postgres and wait until it can actually answer
# a query. Idempotent: run it as many times as you like.
#
#   ./scripts/db-up.sh           # start, wait, print connection strings
#   ./scripts/db-up.sh --quiet   # print only the test connection string
#
# Progress goes to stderr, so stdout carries exactly one machine-readable value
# and `export HIVE_SANDBOX_TEST_DATABASE_URL="$(./scripts/db-up.sh --quiet)"`
# does the right thing.
set -uo pipefail
cd "$(dirname "$0")/.."

quiet=0
[ "${1:-}" = "--quiet" ] && quiet=1

# Podman prints a banner to stderr every time it hands off to an external
# compose provider. The readiness loop calls compose up to 240 times.
export PODMAN_COMPOSE_WARNING_LOGS=false

# Mirrors docker/docker-compose.dev.yml. Change both together.
compose_file="docker/docker-compose.dev.yml"
service="postgres"
db_user="hive_sandbox"
db_password="hive_sandbox"
dev_db="hive_sandbox"
test_db="hive_sandbox_test"
port=55432

log() { echo "$@" >&2; }

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
  log "no compose provider found. Install Podman 5+, or Docker, or set HIVE_SANDBOX_COMPOSE."
  exit 1
fi
compose_cmd+=(-f "$compose_file")

psql_q() { # psql_q <database> <sql> -> trimmed result on stdout
  "${compose_cmd[@]}" exec -T "$service" \
    psql -v ON_ERROR_STOP=1 -U "$db_user" -d "$1" -tAc "$2" 2>/dev/null | tr -d '[:space:]'
}

log "==> compose up (${compose_cmd[0]})"
"${compose_cmd[@]}" up -d >&2
if [ $? -ne 0 ]; then
  log "compose up failed"
  exit 1
fi

log "==> waiting for Postgres to answer a query"
# A listening port is not readiness. During initdb the server accepts local
# connections and then restarts, so poll with a real query instead.
ready=0
for _ in $(seq 1 240); do
  if [ "$(psql_q "$dev_db" 'select 1')" = "1" ]; then
    ready=1
    break
  fi
  sleep 0.5
done
if [ "$ready" -ne 1 ]; then
  log "Postgres did not become ready within 120s. Last 50 log lines:"
  "${compose_cmd[@]}" logs --tail 50 "$service" >&2
  exit 1
fi

if [ "$(psql_q "$dev_db" "select 1 from pg_available_extensions where name = 'vector'")" != "1" ]; then
  log "the running container has no pgvector; expected pgvector/pgvector:pg17"
  exit 1
fi

# The data volume outlives `db-down`, so the test database may or may not exist.
if [ "$(psql_q "$dev_db" "select 1 from pg_database where datname = '$test_db'")" != "1" ]; then
  log "==> creating $test_db"
  psql_q "$dev_db" "create database $test_db owner $db_user" >/dev/null
fi

test_url="postgres://${db_user}:${db_password}@127.0.0.1:${port}/${test_db}?sslmode=disable"
dev_url="postgres://${db_user}:${db_password}@127.0.0.1:${port}/${dev_db}?sslmode=disable"

if [ "$quiet" -eq 1 ]; then
  echo "$test_url"
  exit 0
fi

log ""
log "POSTGRES READY on 127.0.0.1:$port"
log "  dev   $dev_url"
log "  test  $test_url"
log ""
log "Point the integration tests at it for this shell:"
log "  export HIVE_SANDBOX_TEST_DATABASE_URL='$test_url'"
