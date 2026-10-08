# check.ps1 - one-command local verification gate for Potflix.
#
# Runs vet and the test suite from `go\`, and with the flags also rebuilds the
# app and runs the headless self-test. Fails fast: the first failing step stops
# the run with a non-zero exit code, so it is safe to use as a release gate.
#
#   powershell -ExecutionPolicy Bypass -File tools\check.ps1
#   powershell -ExecutionPolicy Bypass -File tools\check.ps1 -Build -Selftest
param(
  [switch]$Build,
  [switch]$Selftest
)
$ErrorActionPreference = 'Stop'

# Push-Location/Set-Location expand '[', ']', '*' and '?' as wildcards, so a
# checkout path containing them aborts with PathNotFound. Escape every path
# handed to a wildcard-aware command. (Literal-path parameters do not need it.)
function Get-Literal([string]$Path) {
  return [WildcardPattern]::Escape($Path)
}

# Resolve the repo root from this script's own path (it lives in tools\), so the
# gate behaves identically from any current directory. Same as tools\build.ps1.
$root = Split-Path (Split-Path $MyInvocation.MyCommand.Path -Parent) -Parent
$goDir = Join-Path $root 'go'

# Preflight: every step below needs `go` on PATH. Exit with a distinct code so a
# missing toolchain is never mistaken for a failing check. This runs before the
# GOTMPDIR probe, which would itself fail as an unresolved command without it.
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  Write-Host 'ERROR: go not found on PATH (Potflix requires Go 1.27+).' -ForegroundColor Red
  exit 2
}

# Go does not create GOTMPDIR; if a temp cleaner removes it, every go build/test
# fails with "creating work dir: ... The system cannot find the file specified".
# Create it when it is set and missing. No machine-specific path is hardcoded -
# the directory is whatever `go env GOTMPDIR` reports. Go resolves a relative
# GOTMPDIR against its own cwd, which this gate sets to go\, so resolve it the
# same way here, and treat an existing file as not-a-directory.
$goTmp = go env GOTMPDIR
if (-not [string]::IsNullOrWhiteSpace($goTmp)) {
  if (-not [IO.Path]::IsPathRooted($goTmp)) {
    $goTmp = [IO.Path]::GetFullPath((Join-Path $goDir $goTmp))
  } else {
    $goTmp = [IO.Path]::GetFullPath($goTmp)
  }
  if (-not (Test-Path -LiteralPath $goTmp -PathType Container)) {
    New-Item -ItemType Directory -Force -Path (Get-Literal $goTmp) | Out-Null
  }
}

# go\embed.go resolves //go:embed relative to go\, so the compiled-in tray
# icon is go\assets\icon.ico, not assets\icon.ico. design\make_logo.py writes
# both; fail here if they ever drift apart again.
Write-Host '[check] icon parity'
$iconRoot = Join-Path $root 'assets\icon.ico'
$iconGo = Join-Path $root 'go\assets\icon.ico'
if (-not (Test-Path -LiteralPath $iconRoot)) {
  Write-Host "CHECK FAILED: icon parity ($iconRoot not found)" -ForegroundColor Red
  exit 1
}
if (-not (Test-Path -LiteralPath $iconGo)) {
  Write-Host "CHECK FAILED: icon parity ($iconGo not found)" -ForegroundColor Red
  exit 1
}
$iconA = [IO.File]::ReadAllBytes($iconRoot)
$iconB = [IO.File]::ReadAllBytes($iconGo)
$iconSame = ($iconA.Length -eq $iconB.Length) -and [Linq.Enumerable]::SequenceEqual($iconA, $iconB)
if (-not $iconSame) {
  Write-Host 'CHECK FAILED: icon parity (assets\icon.ico and go\assets\icon.ico differ; re-run python design\make_logo.py)' -ForegroundColor Red
  exit 1
}

# `go version` is the first toolchain probe. A broken toolchain, or an inherited
# GOFLAGS that go cannot even parse, must stop the run here rather than be
# papered over by a later success.
Write-Host '[check] go version'
go version
if ($LASTEXITCODE -ne 0) {
  Write-Host "CHECK FAILED: go version (exit $LASTEXITCODE)" -ForegroundColor Red
  exit 1
}

# An inherited GOFLAGS (`-n`, `-run=^$`, ...) can turn vet/test into no-ops
# while still reporting success. Ignore it for those two checks, then restore
# exactly what was there before.
$savedGoFlags = $env:GOFLAGS
$goFlagsWereSet = -not [string]::IsNullOrEmpty($env:GOFLAGS)
if ($goFlagsWereSet) {
  Write-Host "note: GOFLAGS='$savedGoFlags' is set and is ignored for this run."
}
$env:GOFLAGS = $null

Push-Location (Get-Literal $goDir)
try {
  Write-Host '[check] go vet ./...'
  go vet ./...
  if ($LASTEXITCODE -ne 0) {
    Write-Host "CHECK FAILED: vet (exit $LASTEXITCODE)" -ForegroundColor Red
    exit 1
  }

  Write-Host '[check] go test ./... -count=1'
  go test ./... -count=1
  if ($LASTEXITCODE -ne 0) {
    Write-Host "CHECK FAILED: test (exit $LASTEXITCODE)" -ForegroundColor Red
    exit 1
  }
} finally {
  Pop-Location
  $env:GOFLAGS = $savedGoFlags
}

if ($Build) {
  Write-Host '[check] build'
  try {
    & (Join-Path $root 'tools\build.ps1')
  } catch {
    # A thrown cmdlet error leaves $LASTEXITCODE stale, so report the exception.
    Write-Host 'CHECK FAILED: build' -ForegroundColor Red
    Write-Host $_.Exception.Message
    exit 1
  }
}

if ($Selftest) {
  $exe = Join-Path $root 'dist\potflix.exe'
  Write-Host '[check] selftest'
  if (-not (Test-Path -LiteralPath $exe)) {
    Write-Host "ERROR: $exe not found - run with -Build to produce it." -ForegroundColor Red
    exit 1
  }

  # Without -Build the exe may predate the current source; warn, never fail.
  if (-not $Build) {
    $exeTime = (Get-Item -LiteralPath $exe).LastWriteTimeUtc
    $newest = Get-ChildItem -LiteralPath $goDir -Recurse -Filter *.go -File |
      Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1
    if ($newest -and $newest.LastWriteTimeUtc -gt $exeTime) {
      [Console]::Error.WriteLine("WARNING: $exe is older than $($newest.FullName); run with -Build to self-test the current source.")
    }
  }

  # The app is built -H=windowsgui and, before it can run the self-test, takes
  # the single-instance lock (go\main.go acquires it at :40-43 and exits 0 when
  # 127.0.0.1:17843 is already held, before the selftest branch at :55-58). So a
  # zero exit code alone cannot tell a real pass from a no-op. Capture both
  # streams and require the success marker `M1 SELFTEST PASS` (go\selftest.go:567).
  #
  # The passing self-test deliberately spawns a second instance to prove the
  # single-instance lock and echoes its message `Potflix is already running.`
  # (go\main.go:29, printed by go\selftest.go:188), so that text is normal here
  # and is never a failure on its own.
  #
  # Use a raw System.Diagnostics.Process, not Start-Process: under Windows
  # PowerShell 5.1 Start-Process with -RedirectStandard* leaves $proc.ExitCode
  # $null, and its pipe handles are not drained concurrently.
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $exe
  $psi.Arguments = '--selftest'
  $psi.WorkingDirectory = $root
  $psi.UseShellExecute = $false
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.CreateNoWindow = $true

  $proc = New-Object System.Diagnostics.Process
  $proc.StartInfo = $psi
  [void]$proc.Start()
  # Start draining both pipes BEFORE waiting; otherwise a child that fills a
  # pipe buffer blocks forever while we block in WaitForExit.
  $outTask = $proc.StandardOutput.ReadToEndAsync()
  $errTask = $proc.StandardError.ReadToEndAsync()
  if (-not $proc.WaitForExit(120000)) {
    try { $proc.Kill() } catch { }
    Write-Host 'CHECK FAILED: selftest (timeout)' -ForegroundColor Red
    exit 1
  }
  $code = $proc.ExitCode
  $outText = $outTask.Result
  $errText = $errTask.Result
  $tail = (($outText + "`n" + $errText) -split "`r?`n" | Where-Object { $_ -ne '' } | Select-Object -Last 15) -join "`n"

  if ($code -ne 0) {
    Write-Host "CHECK FAILED: selftest (exit $code)" -ForegroundColor Red
    if ($tail) { Write-Host $tail }
    exit 1
  }
  if ($outText -notmatch 'M1 SELFTEST PASS') {
    Write-Host "CHECK FAILED: selftest (missing 'M1 SELFTEST PASS')" -ForegroundColor Red
    if ($outText -match 'Potflix is already running\.' -or $errText -match 'Potflix is already running\.') {
      Write-Host 'a Potflix instance already holds 127.0.0.1:17843 - exit it and rerun.' -ForegroundColor Yellow
    }
    if ($tail) { Write-Host $tail }
    exit 1
  }
}

Write-Host 'CHECK PASSED' -ForegroundColor Green
exit 0
