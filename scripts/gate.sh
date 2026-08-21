#!/usr/bin/env bash
# Born-green gate. Run before pushing. Read the OUTPUT, not a piped exit code.
# Order matters: fmt is checked AFTER lint, because lint autofix can reformat.
set -uo pipefail
cd "$(dirname "$0")/.."

failed=()

echo "==> go build ./..."
go build ./... || failed+=("build")

echo "==> go vet ./..."
go vet ./... || failed+=("vet")

echo "==> golangci-lint run"
if command -v golangci-lint >/dev/null 2>&1; then
  golangci-lint run || failed+=("lint")
else
  echo "golangci-lint not found; install with:"
  echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"
  failed+=("lint-missing")
fi

echo "==> gofmt -l ."
unformatted=$(gofmt -l . | grep -v node_modules || true)
if [ -n "$unformatted" ]; then
  echo "unformatted files:"
  echo "$unformatted"
  failed+=("gofmt")
fi

# A gate that reports green over a suite it never ran is worse than no gate,
# because it is the reason somebody stops checking. Without this variable 121
# tests skip themselves, including every test that touches Postgres, and the
# run still says GATE GREEN in about the same wall time ... skipping is fast,
# and -race makes a skipped suite look like a slow one.
#
# So this is a hard precondition rather than a warning. `db-up` is idempotent
# and takes seconds; there is no case where running the gate without a database
# is what somebody meant.
echo "==> database precondition"
if [ -z "${HIVE_SANDBOX_TEST_DATABASE_URL:-}" ]; then
  echo "HIVE_SANDBOX_TEST_DATABASE_URL is not set."
  echo "Every Postgres-backed test would SKIP and the gate would still print GATE GREEN."
  echo "Fix with:"
  echo "  export HIVE_SANDBOX_TEST_DATABASE_URL=\"\$(./scripts/db-up.sh --quiet)\""
  failed+=("database-url-unset")
else
  echo "  $(echo "$HIVE_SANDBOX_TEST_DATABASE_URL" | sed -E 's#://[^@]*@#://***@#')"
fi

echo "==> go test -race ./..."
test_log=$(mktemp)
go test -race -v ./... 2>&1 | tee "$test_log" | grep -E '^(ok|FAIL|\?|---)|^\s+--- (FAIL|SKIP)' || true
grep -qE '^FAIL' "$test_log" && failed+=("test")

# Name what did not run. A skip is a test that told you it was not answering
# the question, and the only way that stays visible is if the gate says so out
# loud every time rather than burying it in -v output nobody passes.
skipped=$(grep -oE '^\s*--- SKIP: [^ ]+' "$test_log" | sed 's/.*SKIP: //' | sort -u || true)
if [ -n "$skipped" ]; then
  echo
  echo "SKIPPED ($(echo "$skipped" | wc -l | tr -d ' ')) ... these did NOT run:"
  echo "$skipped" | sed 's/^/  /'
  echo "Container-image tests need scripts/harness-build.sh and scripts/egress-build.sh."
fi

# A count of what was skipped CANNOT see what was never built.
#
# A test package that does not compile has zero tests rather than skipped ones,
# so it contributes nothing to the number above and "0 skipped" reads identically
# for "everything ran" and "this package was never built". That is not
# hypothetical: internal/store's test package stopped compiling for an hour and
# every Postgres-backed test in it ... the whole grant predicate suite ... was
# absent from main while `go build ./...` stayed green, because test files are
# not part of it.
#
# `go vet ./...` above already fails on it. This is the second reading, because
# the failure mode of the first one being wrong is that the number people trust
# is the number that lies.
if grep -q '\[build failed\]' "$test_log"; then
  echo
  echo "A TEST PACKAGE FAILED TO BUILD ... it ran zero tests and skipped none."
  grep '\[build failed\]' "$test_log" | sed 's/^/  /'
  failed+=("test-build")
fi
rm -f "$test_log"

if [ ${#failed[@]} -gt 0 ]; then
  echo
  echo "GATE RED: ${failed[*]}"
  exit 1
fi
echo
echo "GATE GREEN"
