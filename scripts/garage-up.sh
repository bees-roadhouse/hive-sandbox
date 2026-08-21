#!/usr/bin/env bash
# Bring up the local development Garage and wait until it can actually store an
# object: running, layout applied, bucket created, key granted. Idempotent.
#
#   ./scripts/garage-up.sh           # start, wait, print the exports
#   ./scripts/garage-up.sh --quiet   # print only the S3 credentials, one per line
#
# Progress goes to stderr, so stdout carries exactly the machine-readable values.
# The four lines --quiet prints are, in order: endpoint, bucket, key id, secret.
set -uo pipefail
cd "$(dirname "$0")/.."

quiet=0
[ "${1:-}" = "--quiet" ] && quiet=1

export PODMAN_COMPOSE_WARNING_LOGS=false

# Mirrors docker/docker-compose.garage.yml. Change both together.
compose_file="docker/docker-compose.garage.yml"
env_file="docker/garage/garage.env"
service="garage"
bucket="hive-sandbox"
key_name="hive-sandbox-dev"
port=53900
endpoint="http://127.0.0.1:${port}"

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

# The garage binary sits at / in the image and is NOT on PATH, so it has to be
# called by absolute path. Under Git Bash that argument gets rewritten to a
# Windows path before podman ever sees it and every call fails with "executable
# file not found", which the readiness loop reports as a Garage that never
# started. The two variables are what turn that rewriting off; they are unset
# and inert on Linux.
garage() {
  MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' \
    "${compose_cmd[@]}" exec -T "$service" /garage "$@" 2>/dev/null
}

# The RPC secret is generated per machine and never committed. Regenerating it
# on an existing cluster is harmless here ... one node, and the CLI reads the
# same config the daemon does.
if [ ! -f "$env_file" ]; then
  log "==> generating $env_file"
  mkdir -p "$(dirname "$env_file")"
  if command -v openssl >/dev/null 2>&1; then
    secret=$(openssl rand -hex 32)
  else
    secret=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
  fi
  if [ ${#secret} -ne 64 ]; then
    log "could not generate a 32-byte RPC secret (got ${#secret} hex chars)"
    exit 1
  fi
  printf 'GARAGE_RPC_SECRET=%s\n' "$secret" > "$env_file"
fi

log "==> compose up (${compose_cmd[0]})"
"${compose_cmd[@]}" up -d >&2
if [ $? -ne 0 ]; then
  log "compose up failed"
  exit 1
fi

log "==> waiting for the node to answer"
ready=0
for _ in $(seq 1 120); do
  if garage status | grep -q 'HEALTHY\|NO ROLE ASSIGNED\|Healthy nodes'; then
    ready=1
    break
  fi
  sleep 0.5
done
if [ "$ready" -ne 1 ]; then
  log "Garage did not become reachable within 60s. Last 50 log lines:"
  "${compose_cmd[@]}" logs --tail 50 "$service" >&2
  exit 1
fi

# A node with no layout accepts connections and refuses every write, which is
# the single most confusing state this fixture can be left in.
#
# Three steps, not one: v2.3.0 has no --single-node shortcut, so the node id has
# to be read back and the assignment applied at an explicit version. Zone and
# capacity are both required ... an assign missing either is rejected.
if ! garage layout show | grep -q 'Current cluster layout version: [1-9]'; then
  node_id=$(garage node id -q | cut -d@ -f1)
  if [ -z "$node_id" ]; then
    log "could not read the node id back from Garage"
    exit 1
  fi
  log "==> assigning layout to $node_id"
  if ! garage layout assign -z dev -c 10G "$node_id" >&2; then
    log "layout assign failed"
    exit 1
  fi
  # `layout apply` demands the version it is applying, as a guard against two
  # operators racing. Garage ends `layout show` with the exact command to run;
  # take the number from there rather than assuming 1, so a cluster someone has
  # already touched is fixed rather than rejected.
  version=$(garage layout show | grep -oE -- "--version +[0-9]+" | grep -oE "[0-9]+" | head -n1)
  if [ -z "$version" ]; then
    log "Garage did not propose a layout version to apply"
    exit 1
  fi
  log "==> applying layout version $version"
  if ! garage layout apply --version "$version" >&2; then
    log "layout apply failed"
    exit 1
  fi
fi

if ! garage bucket list | grep -qE "^ *${bucket}\b|[[:space:]]${bucket}[[:space:]]"; then
  log "==> creating bucket $bucket"
  garage bucket create "$bucket" >&2
fi

# Idempotence matters more than it looks: `key create` with an existing name
# makes a SECOND key, so a script that always creates leaks one key per run and
# hands back credentials that differ from the ones already exported.
if garage key list | grep -q "$key_name"; then
  key_info=$(garage key info "$key_name" --show-secret)
else
  log "==> creating key $key_name"
  key_info=$(garage key create "$key_name" 2>/dev/null; garage key info "$key_name" --show-secret)
fi

key_id=$(printf '%s\n' "$key_info" | sed -n 's/.*Key ID: *\([A-Za-z0-9]*\).*/\1/p' | head -n1)
key_secret=$(printf '%s\n' "$key_info" | sed -n 's/.*Secret key: *\([A-Za-z0-9]*\).*/\1/p' | head -n1)
if [ -z "$key_id" ] || [ -z "$key_secret" ]; then
  log "could not read the key back from Garage. It said:"
  printf '%s\n' "$key_info" >&2
  exit 1
fi

log "==> granting $key_name on $bucket"
garage bucket allow --read --write --owner "$bucket" --key "$key_name" >&2

if [ "$quiet" -eq 1 ]; then
  echo "$endpoint"
  echo "$bucket"
  echo "$key_id"
  echo "$key_secret"
  exit 0
fi

log ""
log "GARAGE READY on 127.0.0.1:$port"
log ""
log "Point the S3 driver tests at it for this shell:"
log "  export HIVE_SANDBOX_TEST_S3_ENDPOINT='$endpoint'"
log "  export HIVE_SANDBOX_TEST_S3_BUCKET='$bucket'"
log "  export HIVE_SANDBOX_TEST_S3_ACCESS_KEY_ID='$key_id'"
log "  export HIVE_SANDBOX_TEST_S3_SECRET_ACCESS_KEY='$key_secret'"
log ""
