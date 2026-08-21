# Build the first-party guest apps and drop them where the host tests read them.
#
# The flags are not incidental. Read scripts/guest-build.md before changing one.
$ErrorActionPreference = "Stop"
# Prepend the usual install locations, but only ones that actually exist, so the
# script works on a machine laid out differently and does not hard-code anybody's
# drive letters. Set TINYGO_BIN or WASMOPT_BIN to point somewhere else.
$candidates = @(
    "C:\Program Files\Go\bin",
    $env:TINYGO_BIN,
    $env:WASMOPT_BIN,
    "$env:LOCALAPPDATA\tinygo\bin",
    "G:\tools\tinygo\bin",
    "G:\tools\binaryen\bin"
) | Where-Object { $_ -and (Test-Path $_) }
if ($candidates) { $env:Path = ($candidates -join ";") + ";" + $env:Path }
Set-Location (Split-Path $PSScriptRoot -Parent)

if (-not (Get-Command tinygo -ErrorAction SilentlyContinue)) {
    Write-Host "tinygo not found. Install it from https://github.com/tinygo-org/tinygo/releases" -ForegroundColor Red
    Write-Host "and put binaryen's wasm-opt on PATH too (tinygo shells out to it)."
    exit 1
}
if (-not (Get-Command wasm-opt -ErrorAction SilentlyContinue)) {
    Write-Host "wasm-opt not found. Install binaryen: https://github.com/WebAssembly/binaryen/releases" -ForegroundColor Red
    exit 1
}

$out = "internal/wasmhost/testdata"
New-Item -ItemType Directory -Force -Path $out | Out-Null

$failed = @()
foreach ($app in Get-ChildItem -Directory apps) {
    if (-not (Test-Path (Join-Path $app.FullName "go.mod"))) { continue }
    Write-Host "==> $($app.Name)" -ForegroundColor Cyan
    Push-Location $app.FullName
    tinygo build `
        -target=wasip1 `
        -buildmode=c-shared `
        -scheduler=none `
        -o "../../$out/$($app.Name).wasm" `
        ./
    if (-not $?) { $failed += $app.Name }
    Pop-Location
    $built = Join-Path $out "$($app.Name).wasm"
    if (Test-Path $built) { Get-Item $built | Select-Object Name, Length | Format-Table -AutoSize }
}

if ($failed.Count -gt 0) {
    Write-Host "GUEST BUILD RED: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host "GUEST BUILD GREEN" -ForegroundColor Green
