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
$unformatted = gofmt -l . | Where-Object { $_ -notmatch "node_modules" }
if ($unformatted) {
    Write-Host "unformatted files:" -ForegroundColor Red
    $unformatted | ForEach-Object { Write-Host "  $_" }
    $failed += "gofmt"
}

Write-Host "==> go test -race ./..." -ForegroundColor Cyan
go test -race ./...
if (-not $?) { $failed += "test" }

if ($failed.Count -gt 0) {
    Write-Host ""
    Write-Host "GATE RED: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host ""
Write-Host "GATE GREEN" -ForegroundColor Green
