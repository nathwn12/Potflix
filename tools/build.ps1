# Build Potflix: compile the Go app to dist\potflix.exe.
#
# The Java flow this script used to run (javac against an upstream fat jar
# downloaded from the mutable release tag `v1`, then re-packaging) is RETIRED.
# The Go module in `go\` is the app; the Java sources under `src\` remain only
# as the reference implementation the port was proven against (see DECISION.md).
#
# Flags:
#   -trimpath       embed no absolute checkout paths, so the build is the same
#                   from any directory
#   -buildvcs=false stamp no git revision, so the build is the same at any
#                   commit
#   -H=windowsgui   keep the tray app console-less
#
# Requires Go 1.27+ (stdlib only; no modules are downloaded).
param(
  [string]$Out = ""
)
$ErrorActionPreference = 'Stop'
$root = Split-Path (Split-Path $MyInvocation.MyCommand.Path -Parent) -Parent
if ([string]::IsNullOrWhiteSpace($Out)) { $Out = Join-Path $root 'dist\potflix.exe' }
$Out = [IO.Path]::GetFullPath($Out)
New-Item -ItemType Directory -Force (Split-Path $Out -Parent) | Out-Null

Push-Location (Join-Path $root 'go')
try {
  go build -trimpath -buildvcs=false -ldflags "-H=windowsgui" -o $Out .
  if ($LASTEXITCODE -ne 0) { throw "go build failed (exit $LASTEXITCODE)" }
} finally {
  Pop-Location
}
Write-Host "BUILT: $Out"
Get-Item $Out | Select-Object Name, Length
