# Build the first-party guest apps and drop them where the host tests read them.
# Mirrors build-guests.sh; read scripts/guest-build.md before changing a setting.
$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

$Target = if ($env:TARGET) { $env:TARGET } else { "wasm32-wasip1" }
$Out = if ($env:OUT) { $env:OUT } else { "crates/hive-wasmhost/testdata" }

if (-not (Get-Command cargo -ErrorAction SilentlyContinue)) {
    Write-Host "cargo not found. rustup.rs installs it; rust-toolchain.toml pins the version." -ForegroundColor Red
    exit 1
}
if (-not ((rustup target list --installed) -contains $Target)) {
    Write-Host "==> adding the $Target target"
    rustup target add $Target
}
New-Item -ItemType Directory -Force -Path $Out | Out-Null

# Byte-identical builds wherever they run: see build-guests.sh for why the
# registry and the checkout are remapped to fixed names.
$cargoHome = if ($env:CARGO_HOME) { $env:CARGO_HOME } else { Join-Path $HOME ".cargo" }
$registry = Join-Path $cargoHome "registry/src"
$env:RUSTFLAGS = "$($env:RUSTFLAGS) --remap-path-prefix=$registry=/cargo/registry/src --remap-path-prefix=$(Get-Location)=/hive-sandbox".Trim()

$apps = $args
if (-not $apps) {
    $apps = Get-ChildItem apps -Directory | Where-Object { Test-Path (Join-Path $_.FullName "Cargo.toml") } | ForEach-Object { $_.Name }
}
foreach ($name in $apps) {
    $dir = "apps/$name"
    Write-Host "==> $name"
    Push-Location $dir
    try { cargo build --release --target $Target --quiet; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }
    finally { Pop-Location }
    Copy-Item "$dir/target/$Target/release/$name.wasm" "$Out/$name.wasm" -Force
    Get-Item "$Out/$name.wasm" | Format-Table Name, Length
}
