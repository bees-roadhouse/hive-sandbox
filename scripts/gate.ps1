# Born-green gate. Run before pushing. Read the OUTPUT, not the exit code of a
# piped command. Order matters: fmt is checked AFTER lint, because lint autofix
# can reformat.
$ErrorActionPreference = "Stop"
$env:Path = "C:\Program Files\Go\bin;" + $env:Path
Set-Location (Split-Path $PSScriptRoot -Parent)

$failed = @()

Write-Host "==> go build ./..." -ForegroundColor Cyan
go build ./...
if (-not $?) { $failed += "build" }

Write-Host "==> go vet ./..." -ForegroundColor Cyan
go vet ./...
if (-not $?) { $failed += "vet" }

Write-Host "==> golangci-lint run" -ForegroundColor Cyan
if (Get-Command golangci-lint -ErrorAction SilentlyContinue) {
    golangci-lint run
    if (-not $?) { $failed += "lint" }
} else {
    Write-Host "golangci-lint not found; install with:" -ForegroundColor Yellow
    Write-Host "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"
    $failed += "lint-missing"
}

Write-Host "==> gofmt -l ." -ForegroundColor Cyan
# .gocache is gate-container.sh's own build and module cache, mounted inside
# the tree it formats; without this exclusion the gate flags the toolchain's
# vendored sources as unformatted and reds itself.
$unformatted = gofmt -l . | Where-Object { $_ -notmatch "node_modules" -and $_ -notmatch "^\.gocache/" }
if ($unformatted) {
    Write-Host "unformatted files:" -ForegroundColor Red
    $unformatted | ForEach-Object { Write-Host "  $_" }
    $failed += "gofmt"
}

# A gate that reports green over a suite it never ran is worse than no gate,
# because it is the reason somebody stops checking. Without this variable 121
# tests skip themselves, including every test that touches Postgres, and the
# run still says GATE GREEN in about the same wall time ... skipping is fast,
# and -race makes a skipped suite look like a slow one.
#
# So this is a hard precondition rather than a warning. `db-up` is idempotent
# and takes seconds; there is no case where running the gate without a database
# is what somebody meant.
Write-Host "==> database precondition" -ForegroundColor Cyan
if (-not $env:HIVE_SANDBOX_TEST_DATABASE_URL) {
    Write-Host "HIVE_SANDBOX_TEST_DATABASE_URL is not set." -ForegroundColor Red
    Write-Host "Every Postgres-backed test would SKIP and the gate would still print GATE GREEN."
    Write-Host "Fix with:"
    Write-Host "  `$env:HIVE_SANDBOX_TEST_DATABASE_URL = .\scripts\db-up.ps1 -Quiet"
    $failed += "database-url-unset"
} else {
    Write-Host "  $($env:HIVE_SANDBOX_TEST_DATABASE_URL -replace '://[^@]*@', '://***@')"
}

Write-Host "==> go test -race ./..." -ForegroundColor Cyan
$testOutput = go test -race -v ./... 2>&1 | Tee-Object -Variable captured | Where-Object {
    $_ -match "^(ok|FAIL|---|\?)" -or $_ -match "^\s+--- (FAIL|SKIP)"
}
$testOutput | ForEach-Object { Write-Host $_ }
if (-not $?) { $failed += "test" }
if ($captured -match "^FAIL") { $failed += "test" }

# Name what did not run. A skip is a test that told you it was not answering
# the question, and the only way that stays visible is if the gate says so out
# loud every time rather than burying it in -v output nobody passes.
$skipped = @($captured | Select-String -Pattern "^\s*--- SKIP: (\S+)" | ForEach-Object {
    $_.Matches[0].Groups[1].Value
})
if ($skipped.Count -gt 0) {
    Write-Host ""
    Write-Host "SKIPPED ($($skipped.Count)) ... these did NOT run:" -ForegroundColor Yellow
    $skipped | Sort-Object -Unique | ForEach-Object { Write-Host "  $_" -ForegroundColor Yellow }
    Write-Host "Container-image tests need scripts/harness-build.ps1 and scripts/egress-build.ps1." -ForegroundColor Yellow
}

# A count of what was skipped CANNOT see what was never built.
#
# A test package that does not compile has zero tests rather than skipped ones,
# so it contributes nothing to the number above and "0 skipped" reads identically
# for "everything ran" and "this package was never built". That is not
# hypothetical: internal/store's test package stopped compiling for an hour and
# every Postgres-backed test in it ... the whole grant predicate suite ... was
# absent from main while go build ./... stayed green, because test files are not
# part of it.
#
# `go vet ./...` above already fails on it. This is the second reading, because
# the failure mode of the first one being wrong is that the number people trust
# is the number that lies.
if ($captured -match "\[build failed\]") {
    Write-Host ""
    Write-Host "A TEST PACKAGE FAILED TO BUILD ... it ran zero tests and skipped none." -ForegroundColor Red
    $captured | Where-Object { $_ -match "\[build failed\]" } | ForEach-Object { Write-Host "  $_" -ForegroundColor Red }
    $failed += "test-build"
}

if ($failed.Count -gt 0) {
    Write-Host ""
    Write-Host "GATE RED: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host ""
Write-Host "GATE GREEN" -ForegroundColor Green
