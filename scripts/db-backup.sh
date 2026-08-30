#!/usr/bin/env bash
# Dump the database before a migration, and keep the last N dumps.
#
#   ./scripts/db-backup.sh                      # dump, keep 7
#   ./scripts/db-backup.sh --keep 14            # keep more
#   ./scripts/db-backup.sh --dir /var/backups   # somewhere else
#   ./scripts/db-backup.sh --url postgres://... # a database other than the dev one
#
# WHY THIS EXISTS
#
# Migrations are one-way. `Migrate` applies every embedded migration that has
# not run yet and refuses one whose checksum changed after it was applied;
# there is no down-migration and no rollback. So the ONLY way back from a
# migration that turns out to be wrong is a dump taken before it ran, and the
# moment to take it is the moment before -- not on a nightly schedule that may
# have last run twenty hours ago.
#
# The dump is named for the schema version it captures, so the name answers the
# question you actually have in an incident: "what does this restore me to?"
#
#   0001_20260830T164500Z.dump   <- the database AS OF migration 0001
#
# A timestamp alone would not: two dumps an hour apart may be the same schema
# or two different ones, and you would have to restore one to find out.
#
# WHY IT SHELLS OUT TO A CONTAINER
#
# The daemon image is distroless/static -- no shell, no pg_dump, nothing. That
# is a deliberate posture and adding a Postgres client to it to enable backups
# would trade a permanent increase in attack surface for an occasional
# convenience. So the dump runs in a throwaway postgres image instead, which
# also guarantees the client version matches the server it is dumping.
set -uo pipefail
cd "$(dirname "$0")/.."

dir="backups"
keep=7
url="${HIVE_SANDBOX_DATABASE_URL:-}"
image="docker.io/pgvector/pgvector:pg17"

while [ $# -gt 0 ]; do
  case "$1" in
    --dir)   dir="$2";   shift 2 ;;
    --keep)  keep="$2";  shift 2 ;;
    --url)   url="$2";   shift 2 ;;
    --image) image="$2"; shift 2 ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

log() { printf '%s\n' "$*" >&2; }

if [ -z "$url" ]; then
  # The dev database is the default target, and db-up.sh is the thing that
  # knows its connection string.
  if [ -x ./scripts/db-up.sh ]; then
    url="$(./scripts/db-up.sh --quiet 2>/dev/null | tail -1)"
    # db-up prints the TEST database; the dev one is what a daemon migrates.
    url="${url/hive_sandbox_test/hive_sandbox}"
  fi
fi
if [ -z "$url" ]; then
  log "no database: pass --url or set HIVE_SANDBOX_DATABASE_URL"
  exit 2
fi

case "$keep" in
  ''|*[!0-9]*) log "--keep must be a whole number"; exit 2 ;;
esac
if [ "$keep" -lt 1 ]; then
  log "--keep must be at least 1; refusing to prune every backup"
  exit 2
fi

# Prefer a local client, fall back to a container. Everything in this repo can
# be built and tested in containers precisely so a bare machine is enough.
runner=()
if command -v pg_dump >/dev/null 2>&1 && command -v psql >/dev/null 2>&1; then
  runner=(local)
elif command -v podman >/dev/null 2>&1; then
  runner=(podman run --rm --network=host -i "$image")
elif command -v docker >/dev/null 2>&1; then
  runner=(docker run --rm --network=host -i "$image")
else
  log "need pg_dump, or podman/docker to run it in a container"
  exit 2
fi

run_psql() {
  if [ "${runner[0]}" = "local" ]; then
    psql "$url" -tAc "$1" 2>/dev/null
  else
    "${runner[@]}" psql "$url" -tAc "$1" 2>/dev/null
  fi
}

# The version we are ABOUT to move off. A database with no schema_migrations
# table has never been migrated, which is worth capturing under 0000 rather
# than refusing: an empty database is a legitimate thing to have a dump of.
version="$(run_psql 'SELECT lpad(max(version)::text, 4, '"'"'0'"'"') FROM schema_migrations' | tr -d '[:space:]')"
if [ -z "$version" ]; then
  version="0000"
  log "==> no schema_migrations; recording this dump as 0000"
fi

mkdir -p "$dir"
# UTC, and sortable. Local time would reorder the backups twice a year, which
# is exactly when you least want to be reasoning about which is newest.
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
out="$dir/${version}_${stamp}.dump"

log "==> dumping schema version $version to $out"
# -Fc (custom format): compressed, and restorable selectively with pg_restore.
# A plain SQL dump would be readable and much larger, and the thing you do with
# it in an incident is feed it to pg_restore anyway.
if [ "${runner[0]}" = "local" ]; then
  pg_dump -Fc "$url" > "$out"
else
  "${runner[@]}" pg_dump -Fc "$url" > "$out"
fi
status=$?

# A truncated dump is worse than none: it restores far enough to look like it
# worked. Fail loudly and leave nothing behind that could be mistaken for a
# good backup.
if [ $status -ne 0 ] || [ ! -s "$out" ]; then
  rm -f "$out"
  log "dump failed; removed the partial file"
  exit 1
fi
log "    $(du -h "$out" | cut -f1)"

# Prune by modification time, newest kept. Sorting by NAME would be wrong the
# first time a version number reaches five digits, and by then nobody would be
# looking at this script.
mapfile -t old < <(ls -1t "$dir"/*.dump 2>/dev/null | tail -n +"$((keep + 1))")
if [ "${#old[@]}" -gt 0 ]; then
  log "==> pruning $((${#old[@]})) dump(s) beyond the newest $keep"
  for f in "${old[@]}"; do
    log "    rm $(basename "$f")"
    rm -f "$f"
  done
fi

printf '%s\n' "$out"
