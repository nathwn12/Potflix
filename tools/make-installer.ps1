# Build the Potflix installer: multi-size icon + ISCC compile of potflix.iss.
#
# Produces dist\Potflix-Setup.exe (per-user, no UAC). Requires Pillow
# (`python`, tested with 11.2.1) and Inno Setup 6 (`ISCC` on PATH).
$ErrorActionPreference = 'Stop'

$root = Split-Path (Split-Path $MyInvocation.MyCommand.Path -Parent) -Parent
$toolsDir = Join-Path $root 'tools'
$srcIcon = Join-Path $root 'assets\icon.ico'
$dstIcon = Join-Path $root 'build\potflix.ico'
$iss = Join-Path $toolsDir 'potflix.iss'

New-Item -ItemType Directory -Force (Split-Path $dstIcon -Parent) | Out-Null

# Multi-size icon for the installer + shortcuts. Pillow reads the 256x256
# source .ico and writes all sizes Windows may request into build\.
$pyCode = @'
from PIL import Image
src = r'__SRC__'
dst = r'__DST__'
img = Image.open(src)
img.save(dst, sizes=[(16, 16), (20, 20), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)])
print('wrote ' + dst)
'@ -replace '__SRC__', $srcIcon -replace '__DST__', $dstIcon
$pyCode | python -
if ($LASTEXITCODE -ne 0) { throw "icon generation failed (exit $LASTEXITCODE)" }

$iscc = Get-Command ISCC -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Source -ErrorAction SilentlyContinue
if ([string]::IsNullOrWhiteSpace($iscc)) { $iscc = 'C:\Program Files (x86)\Inno Setup 6\ISCC.exe' }
& $iscc $iss
if ($LASTEXITCODE -ne 0) { throw "ISCC failed (exit $LASTEXITCODE)" }

$setup = Join-Path $root 'dist\Potflix-Setup.exe'
Write-Host "BUILT: $setup"
Get-Item $setup | Select-Object Name, Length
