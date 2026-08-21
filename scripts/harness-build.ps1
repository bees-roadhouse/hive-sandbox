# Build the harness images and record the digests runs pin to.
#
#   .\scripts\harness-build.ps1              # build all three
#   .\scripts\harness-build.ps1 claude       # build one, relock what exists
#
# Writes docker/harness/digests.json. That file is the runtime pin (D12.5): a
# run records the digest it used, and nothing runs a floating tag.
#
# The lockfile is derived from podman's actual image state rather than merged
# with its own previous contents, so it cannot drift from what is on disk. It is
# gitignored, because a locally built image has a machine-local digest ... the
# committed pin is the version ARGs in the Containerfile.
param([string[]]$Runtimes)

$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

$env:PODMAN_COMPOSE_WARNING_LOGS = "false"

$Containerfile = "docker/harness/Containerfile"
$Lockfile = "docker/harness/digests.json"
$Repo = "localhost/hive-sandbox-harness"
$AllRuntimes = @("claude", "codex", "opencode")

if (-not $Runtimes -or $Runtimes.Count -eq 0) { $Runtimes = $AllRuntimes }

if (-not (Get-Command podman -ErrorAction SilentlyContinue)) {
    Write-Host "podman not found. The harness builds and runs under rootless Podman (D7)." -ForegroundColor Red
    exit 1
}

foreach ($runtime in $Runtimes) {
    if ($AllRuntimes -notcontains $runtime) {
        Write-Host "unknown runtime '$runtime'; expected one of: $($AllRuntimes -join ', ')" -ForegroundColor Red
        exit 1
    }
}

foreach ($runtime in $Runtimes) {
    Write-Host "==> building ${Repo}:${runtime}" -ForegroundColor Cyan
    # --target selects the entrypoint stage. The base layer builds once and is
    # cached across all three.
    podman build --target $runtime --tag "${Repo}:${runtime}" -f $Containerfile docker/harness
    if ($LASTEXITCODE -ne 0) {
        Write-Host "build failed for $runtime" -ForegroundColor Red
        exit 1
    }
}

Write-Host "==> recording digests" -ForegroundColor Cyan

# A Go template with quoted map keys has to reach podman with those quotes
# intact; Windows PowerShell strips bare ones when calling a native command.
$VersionFormat = '{{index .Labels \"org.beesroadhouse.harness.cli-version\"}}'

# Redirecting a native command's stderr is a terminating NativeCommandError
# under `Stop`, and a missing tag is an ordinary skip here.
$ErrorActionPreference = "Continue"

$entries = @()
foreach ($runtime in $AllRuntimes) {
    $tag = "${Repo}:${runtime}"
    $digest = (podman image inspect $tag --format '{{.Digest}}' 2>$null | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $digest) { continue }

    $cliVersion = (podman image inspect $tag --format $VersionFormat 2>$null | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $cliVersion) { $cliVersion = "unknown" }

    $entries += "    `"$runtime`": { `"digest`": `"$digest`", `"cli_version`": `"$cliVersion`" }"
    Write-Host "    $runtime  $digest  ($cliVersion)"
}

$json = @(
    "{",
    "  `"repository`": `"$Repo`",",
    "  `"runtimes`": {",
    ($entries -join ",`n"),
    "  }",
    "}"
) -join "`n"

# Set-Content -Encoding utf8 writes a BOM in Windows PowerShell, and a BOM makes
# encoding/json reject the file.
[System.IO.File]::WriteAllText(
    (Join-Path (Get-Location) $Lockfile),
    $json + "`n",
    (New-Object System.Text.UTF8Encoding($false)))

Write-Host ""
Write-Host "HARNESS IMAGES BUILT" -ForegroundColor Green
Write-Host "  pins recorded in $Lockfile"
