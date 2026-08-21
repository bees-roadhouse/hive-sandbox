# Stop the local development Postgres.
#
#   .\scripts\db-down.ps1          # stop and remove the container, keep the data
#   .\scripts\db-down.ps1 -Purge   # also delete the volume, so db-up starts empty
param([switch]$Purge)

$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

$env:PODMAN_COMPOSE_WARNING_LOGS = "false"

$ComposeFile = "docker/docker-compose.dev.yml"

function Resolve-ComposeArgs {
    if ($env:HIVE_SANDBOX_COMPOSE) { return @($env:HIVE_SANDBOX_COMPOSE -split '\s+') }
    if (Get-Command podman -ErrorAction SilentlyContinue) { return @("podman", "compose") }
    if (Get-Command docker -ErrorAction SilentlyContinue) { return @("docker", "compose") }
    if (Get-Command podman-compose -ErrorAction SilentlyContinue) { return @("podman-compose") }
    throw "no compose provider found. Install Podman 5+, or Docker Desktop, or set HIVE_SANDBOX_COMPOSE."
}

$composeCmd = Resolve-ComposeArgs
$exe = $composeCmd[0]
$base = @()
if ($composeCmd.Count -gt 1) { $base = $composeCmd[1..($composeCmd.Count - 1)] }
$base += @("-f", $ComposeFile)

$rest = @("down")
if ($Purge) { $rest += "-v" }

Write-Host "==> compose $($rest -join ' ')" -ForegroundColor Cyan
& $exe @base @rest
if ($LASTEXITCODE -ne 0) {
    Write-Host "compose down failed" -ForegroundColor Red
    exit 1
}

Write-Host ""
if ($Purge) {
    Write-Host "POSTGRES DOWN, volume deleted" -ForegroundColor Green
} else {
    Write-Host "POSTGRES DOWN, volume hive-sandbox-pgdata kept (-Purge deletes it)" -ForegroundColor Green
}
