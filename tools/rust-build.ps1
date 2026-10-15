# Potflix (Rust) -- build the tray app to dist\potflix.exe.
#
# The Rust workspace is the sole implementation and produces the shipped
# binary (the Go/Java reference era is retired; its history remains in git).
#
# Builds the whole crate workspace (`rust\Cargo.toml`, one member crate per
# package of the retired Go module under `rust\crates\`) and copies the app
# binary out.
#
# Flags:
#   --release  optimised binary
#   --locked   build exactly the committed rust\Cargo.lock (never silently
#              re-resolve or upgrade a pinned dependency)
param(
  [string]$Out = ""
)
$ErrorActionPreference = 'Stop'

$root = Split-Path (Split-Path $MyInvocation.MyCommand.Path -Parent) -Parent
if ([string]::IsNullOrWhiteSpace($Out)) { $Out = Join-Path $root 'dist\potflix.exe' }
$Out = [IO.Path]::GetFullPath($Out)
New-Item -ItemType Directory -Force (Split-Path $Out -Parent) | Out-Null

Push-Location (Join-Path $root 'rust')
try {
  cargo build --workspace --release --locked
  if ($LASTEXITCODE -ne 0) { throw "cargo build failed (exit $LASTEXITCODE)" }
  Copy-Item (Join-Path $root 'rust\target\release\potflix.exe') $Out -Force
} finally {
  Pop-Location
}
Write-Host "BUILT: $Out"
Get-Item $Out | Select-Object Name, Length
