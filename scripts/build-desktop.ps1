# Check the desktop client's webview-free packages on Windows.
#
#   .\scripts\build-desktop.ps1            # vet + test of internal/ and ui/
#
# The windowed binary is a Linux deliverable in Phase A ... Wails links
# WebKit2GTK, which Windows does not provide. These checks still run here
# because nothing under desktop/internal imports a GUI toolkit; that is the
# rule that makes them portable, and this script is how the rule stays honest.
$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)
if ($env:Path -notmatch "Go\\bin") { $env:Path = "C:\Program Files\Go\bin;$env:Path" }

$failed = @()

Write-Host "==> desktop: go vet (webview-free packages)" -ForegroundColor Cyan
Push-Location desktop
try {
    go vet ./internal/... ./ui
    if ($LASTEXITCODE -ne 0) { $failed += "vet" }

    Write-Host "==> desktop: go test -race (webview-free packages)" -ForegroundColor Cyan
    go test -race -count=1 -timeout 120s ./internal/...
    if ($LASTEXITCODE -ne 0) { $failed += "test" }
} finally {
    Pop-Location
}

if ($failed.Count -gt 0) { Write-Host "GATE RED: $($failed -join ' ')" -ForegroundColor Red; exit 1 }
Write-Host "GATE GREEN (headless)" -ForegroundColor Green
