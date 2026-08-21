# Bring up the local development Postgres and wait until it can actually answer
# a query. Idempotent: run it as many times as you like.
#
#   .\scripts\db-up.ps1          # start, wait, print connection strings
#   .\scripts\db-up.ps1 -Quiet   # print only the test connection string
#
# Under -Quiet the connection string is the only thing on the success stream, so
# `$env:HIVE_SANDBOX_TEST_DATABASE_URL = .\scripts\db-up.ps1 -Quiet` works.
param([switch]$Quiet)

$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

# Podman prints a banner to stderr every time it hands off to an external
# compose provider. The readiness loop calls compose ~240 times.
$env:PODMAN_COMPOSE_WARNING_LOGS = "false"

# Mirrors docker/docker-compose.dev.yml. Change both together.
$ComposeFile = "docker/docker-compose.dev.yml"
$Service = "postgres"
$User = "hive_sandbox"
$Password = "hive_sandbox"
$DevDb = "hive_sandbox"
$TestDb = "hive_sandbox_test"
$Port = 55432

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

function Invoke-Compose([string[]]$Rest) {
    & $exe @base @Rest
}

# Runs SQL inside the container as the owning role. Returns trimmed stdout, or
# $null when psql could not run at all.
function Invoke-Psql([string]$Database, [string]$Sql) {
    # Redirecting a native command's stderr wraps each line in an ErrorRecord in
    # Windows PowerShell, which is terminating under `Stop`. Drop to Continue so
    # a not-ready-yet psql is just a failed poll.
    $ErrorActionPreference = "Continue"
    $out = Invoke-Compose @("exec", "-T", $Service, "psql", "-v", "ON_ERROR_STOP=1", "-U", $User, "-d", $Database, "-tAc", $Sql) 2>$null
    if ($LASTEXITCODE -ne 0) { return $null }
    return ($out | Out-String).Trim()
}

Write-Host "==> compose up ($exe $($base -join ' '))" -ForegroundColor Cyan
# Compose writes to the success stream, which -Quiet promises to keep clean.
if ($Quiet) { Invoke-Compose @("up", "-d") | Out-Null } else { Invoke-Compose @("up", "-d") }
if ($LASTEXITCODE -ne 0) {
    Write-Host "compose up failed" -ForegroundColor Red
    exit 1
}

Write-Host "==> waiting for Postgres to answer a query" -ForegroundColor Cyan
# A listening port is not readiness. During initdb the server accepts local
# connections and then restarts, so poll with a real query instead.
$deadline = (Get-Date).AddSeconds(120)
$ready = $false
while ((Get-Date) -lt $deadline) {
    if ((Invoke-Psql $DevDb "select 1") -eq "1") { $ready = $true; break }
    Start-Sleep -Milliseconds 500
}
if (-not $ready) {
    Write-Host "Postgres did not become ready within 120s. Last 50 log lines:" -ForegroundColor Red
    Invoke-Compose @("logs", "--tail", "50", $Service)
    exit 1
}

if ((Invoke-Psql $DevDb "select 1 from pg_available_extensions where name = 'vector'") -ne "1") {
    Write-Host "the running container has no pgvector; expected pgvector/pgvector:pg17" -ForegroundColor Red
    exit 1
}

# The data volume outlives `db-down`, so the test database may or may not exist.
if ((Invoke-Psql $DevDb "select 1 from pg_database where datname = '$TestDb'") -ne "1") {
    Write-Host "==> creating $TestDb" -ForegroundColor Cyan
    $null = Invoke-Psql $DevDb "create database $TestDb owner $User"
}

$testUrl = "postgres://${User}:${Password}@127.0.0.1:${Port}/${TestDb}?sslmode=disable"
$devUrl = "postgres://${User}:${Password}@127.0.0.1:${Port}/${DevDb}?sslmode=disable"

if ($Quiet) {
    Write-Output $testUrl
    exit 0
}

Write-Host ""
Write-Host "POSTGRES READY on 127.0.0.1:$Port" -ForegroundColor Green
Write-Host "  dev   $devUrl"
Write-Host "  test  $testUrl"
Write-Host ""
Write-Host "Point the integration tests at it for this shell:"
Write-Host "  `$env:HIVE_SANDBOX_TEST_DATABASE_URL = '$testUrl'"
