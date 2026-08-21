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

echo "==> go test -race ./..."
go test -race ./... || failed+=("test")

if [ ${#failed[@]} -gt 0 ]; then
  echo
  echo "GATE RED: ${failed[*]}"
  exit 1
fi
echo
echo "GATE GREEN"
