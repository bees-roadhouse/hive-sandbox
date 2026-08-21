# Bring up the local development Garage and wait until it can actually store an
# object: running, layout applied, bucket created, key granted. Idempotent.
#
#   .\scripts\garage-up.ps1          # start, wait, print the exports
#   .\scripts\garage-up.ps1 -Quiet   # print only the S3 credentials, one per line
#
# Under -Quiet the only things on the success stream are four lines, in order:
# endpoint, bucket, key id, secret.
param([switch]$Quiet)

$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

$env:PODMAN_COMPOSE_WARNING_LOGS = "false"

# Mirrors docker/docker-compose.garage.yml. Change both together.
$ComposeFile = "docker/docker-compose.garage.yml"
$EnvFile = "docker/garage/garage.env"
$Service = "garage"
$Bucket = "hive-sandbox"
$KeyName = "hive-sandbox-dev"
$Port = 53900
$Endpoint = "http://127.0.0.1:$Port"

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

# Runs the garage CLI inside the container. Returns trimmed stdout, or $null when
# it could not run at all.
function Invoke-Garage([string[]]$GarageArgs) {
    # Redirecting a native command's stderr wraps each line in an ErrorRecord in
    # Windows PowerShell, which is terminating under `Stop`. Drop to Continue so
    # a not-ready-yet node is just a failed poll.
    $ErrorActionPreference = "Continue"
    $out = Invoke-Compose (@("exec", "-T", $Service, "/garage") + $GarageArgs) 2>$null
    if ($LASTEXITCODE -ne 0) { return $null }
    return ($out | Out-String).Trim()
}

# The RPC secret is generated per machine and never committed. A literal in the
# repo would be a credential in a public repository, and "it is only for dev" is
# the sentence people write before it turns up somewhere it is not.
if (-not (Test-Path $EnvFile)) {
    Write-Host "==> generating $EnvFile" -ForegroundColor Cyan
    $null = New-Item -ItemType Directory -Force (Split-Path $EnvFile -Parent)
    $bytes = New-Object byte[] 32
    # RNGCryptoServiceProvider rather than RandomNumberGenerator.Fill: Windows
    # PowerShell 5.1 runs on .NET Framework, where Fill does not exist. Not
    # Get-Random either ... that is a seeded PRNG.
    $rng = [System.Security.Cryptography.RNGCryptoServiceProvider]::new()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    $secret = -join ($bytes | ForEach-Object { $_.ToString("x2") })
    # Written through .NET rather than Out-File. Every PowerShell 5.1 writer here
    # emits UTF-8 WITH a BOM, and -Encoding utf8NoBOM does not exist in 5.1 at
    # all; the BOM would become part of the first env var's name and Garage would
    # refuse to start with no useful message.
    [System.IO.File]::WriteAllText(
        (Join-Path (Get-Location) $EnvFile),
        "GARAGE_RPC_SECRET=$secret`n",
        (New-Object System.Text.UTF8Encoding($false)))
}

Write-Host "==> compose up ($exe $($base -join ' '))" -ForegroundColor Cyan
if ($Quiet) { Invoke-Compose @("up", "-d") | Out-Null } else { Invoke-Compose @("up", "-d") }
if ($LASTEXITCODE -ne 0) {
    Write-Host "compose up failed" -ForegroundColor Red
    exit 1
}

Write-Host "==> waiting for the node to answer" -ForegroundColor Cyan
$deadline = (Get-Date).AddSeconds(60)
$ready = $false
while ((Get-Date) -lt $deadline) {
    $status = Invoke-Garage @("status")
    if ($status -and ($status -match "HEALTHY|NO ROLE ASSIGNED|Healthy nodes")) { $ready = $true; break }
    Start-Sleep -Milliseconds 500
}
if (-not $ready) {
    Write-Host "Garage did not become reachable within 60s. Last 50 log lines:" -ForegroundColor Red
    Invoke-Compose @("logs", "--tail", "50", $Service)
    exit 1
}

# A node with no layout accepts connections and refuses every write, which is the
# single most confusing state this fixture can be left in.
#
# Three steps, not one: v2.3.0 has no --single-node shortcut, so the node id has
# to be read back and the assignment applied at an explicit version. Zone and
# capacity are both required ... an assign missing either is rejected.
$layout = Invoke-Garage @("layout", "show")
if (-not ($layout -match "Current cluster layout version: [1-9]")) {
    $nodeId = (Invoke-Garage @("node", "id", "-q")) -split "@" | Select-Object -First 1
    if (-not $nodeId) {
        Write-Host "could not read the node id back from Garage" -ForegroundColor Red
        exit 1
    }
    Write-Host "==> assigning layout to $nodeId" -ForegroundColor Cyan
    if ($null -eq (Invoke-Garage @("layout", "assign", "-z", "dev", "-c", "10G", $nodeId))) {
        Write-Host "layout assign failed" -ForegroundColor Red
        exit 1
    }
    # `layout apply` demands the version it is applying, as a guard against two
    # operators racing. Garage ends `layout show` with the exact command to run;
    # take the number from there rather than assuming 1, so a cluster someone has
    # already touched is fixed rather than rejected.
    $proposed = [regex]::Match((Invoke-Garage @("layout", "show")), "--version\s+(\d+)")
    if (-not $proposed.Success) {
        Write-Host "Garage did not propose a layout version to apply" -ForegroundColor Red
        exit 1
    }
    Write-Host "==> applying layout version $($proposed.Groups[1].Value)" -ForegroundColor Cyan
    if ($null -eq (Invoke-Garage @("layout", "apply", "--version", $proposed.Groups[1].Value))) {
        Write-Host "layout apply failed" -ForegroundColor Red
        exit 1
    }
}

$buckets = Invoke-Garage @("bucket", "list")
if (-not ($buckets -match "\b$([regex]::Escape($Bucket))\b")) {
    Write-Host "==> creating bucket $Bucket" -ForegroundColor Cyan
    $null = Invoke-Garage @("bucket", "create", $Bucket)
}

# Idempotence matters more than it looks: `key create` with an existing name
# makes a SECOND key, so a script that always creates leaks one key per run and
# hands back credentials that differ from the ones already exported.
$keys = Invoke-Garage @("key", "list")
if (-not ($keys -match [regex]::Escape($KeyName))) {
    Write-Host "==> creating key $KeyName" -ForegroundColor Cyan
    $null = Invoke-Garage @("key", "create", $KeyName)
}
$keyInfo = Invoke-Garage @("key", "info", $KeyName, "--show-secret")

$keyId = ([regex]::Match($keyInfo, "Key ID:\s*([A-Za-z0-9]+)")).Groups[1].Value
$keySecret = ([regex]::Match($keyInfo, "Secret key:\s*([A-Za-z0-9]+)")).Groups[1].Value
if (-not $keyId -or -not $keySecret) {
    Write-Host "could not read the key back from Garage. It said:" -ForegroundColor Red
    Write-Host $keyInfo
    exit 1
}

Write-Host "==> granting $KeyName on $Bucket" -ForegroundColor Cyan
$null = Invoke-Garage @("bucket", "allow", "--read", "--write", "--owner", $Bucket, "--key", $KeyName)

if ($Quiet) {
    Write-Output $Endpoint
    Write-Output $Bucket
    Write-Output $keyId
    Write-Output $keySecret
    exit 0
}

Write-Host ""
Write-Host "GARAGE READY on 127.0.0.1:$Port" -ForegroundColor Green
Write-Host ""
Write-Host "Point the S3 driver tests at it for this shell:"
Write-Host "  `$env:HIVE_SANDBOX_TEST_S3_ENDPOINT = '$Endpoint'"
Write-Host "  `$env:HIVE_SANDBOX_TEST_S3_BUCKET = '$Bucket'"
Write-Host "  `$env:HIVE_SANDBOX_TEST_S3_ACCESS_KEY_ID = '$keyId'"
Write-Host "  `$env:HIVE_SANDBOX_TEST_S3_SECRET_ACCESS_KEY = '$keySecret'"
Write-Host ""
