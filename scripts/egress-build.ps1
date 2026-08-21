# Build the egress proxy image and record its digest.
#
#   .\scripts\egress-build.ps1
#
# Writes docker/egress/digest.json. Its own file rather than a block spliced
# into the harness lockfile: two independent build scripts editing one JSON file
# is a merge problem nobody asked for.
#
# Same caveat as the harness images: a locally built image has a machine-local
# digest, so this is build output rather than a committed pin.
$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

$env:PODMAN_COMPOSE_WARNING_LOGS = "false"

$Containerfile = "docker/egress/Containerfile"
$Lockfile = "docker/egress/digest.json"
$Repo = "localhost/hive-sandbox-egress"
$Tag = "${Repo}:latest"

if (-not (Get-Command podman -ErrorAction SilentlyContinue)) {
    Write-Host "podman not found. The proxy builds and runs under rootless Podman (D7)." -ForegroundColor Red
    exit 1
}

$version = "dev"
if (Get-Command git -ErrorAction SilentlyContinue) {
    $ErrorActionPreference = "Continue"
    $described = (git describe --tags --always --dirty 2>$null | Out-String).Trim()
    $ErrorActionPreference = "Stop"
    if ($described) { $version = $described }
}

Write-Host "==> building $Tag (version $version)" -ForegroundColor Cyan
# Context is the repo root: the build stage compiles the whole module.
podman build --target proxy --tag $Tag --build-arg "VERSION=$version" -f $Containerfile .
if ($LASTEXITCODE -ne 0) {
    Write-Host "build failed" -ForegroundColor Red
    exit 1
}

$ErrorActionPreference = "Continue"
$digest = (podman image inspect $Tag --format '{{.Digest}}' 2>$null | Out-String).Trim()
$ErrorActionPreference = "Stop"
if (-not $digest) {
    Write-Host "could not read a digest for $Tag" -ForegroundColor Red
    exit 1
}

$json = @(
    "{",
    "  `"repository`": `"$Repo`",",
    "  `"digest`": `"$digest`",",
    "  `"version`": `"$version`"",
    "}"
) -join "`n"

# Set-Content -Encoding utf8 writes a BOM in Windows PowerShell, and a BOM makes
# encoding/json reject the file.
[System.IO.File]::WriteAllText(
    (Join-Path (Get-Location) $Lockfile),
    $json + "`n",
    (New-Object System.Text.UTF8Encoding($false)))

Write-Host "    egress  $digest  ($version)"
Write-Host ""
Write-Host "EGRESS PROXY IMAGE BUILT" -ForegroundColor Green
Write-Host "  pin recorded in $Lockfile"
