# Build Potflix without Gradle: compile against the upstream fat jar, then
# repackage (our classes + Netflix icon override the upstream ones).
$ErrorActionPreference = 'Stop'
$root = Split-Path (Split-Path $MyInvocation.MyCommand.Path -Parent) -Parent
$orig = "$env:TEMP\pot-rpc\PotPlayer-DiscordRPC.jar"
if (!(Test-Path $orig)) {
  New-Item -ItemType Directory -Force "$env:TEMP\pot-rpc" | Out-Null
  Invoke-WebRequest -Uri 'https://github.com/ayunami2000/PotPlayer-DiscordRPC/releases/download/v1/PotPlayer-DiscordRPC.jar' -OutFile $orig
}
$classes = "$root\build\classes"
Remove-Item $classes -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $classes, "$root\dist" | Out-Null

javac -encoding UTF-8 -cp $orig -d $classes `
  "$root\src\main\java\me\potflix\rpc\Main.java"
if ($LASTEXITCODE -ne 0) { throw 'compile failed' }

Copy-Item $orig "$root\dist\Potflix.jar" -Force
# overlay: our classes + new icon.png (replaces upstream icon)
$stage = "$root\build\stage"
Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $stage | Out-Null
Copy-Item "$classes\me" "$stage\" -Recurse -Force
Copy-Item "$root\src\main\resources\icon.png" "$stage\icon.png" -Force
Push-Location $stage
jar uf "$root\dist\Potflix.jar" me icon.png
if ($LASTEXITCODE -ne 0) { throw 'repack failed' }
Pop-Location
# point manifest at our Main
jar ufe "$root\dist\Potflix.jar" me.potflix.rpc.Main
if ($LASTEXITCODE -ne 0) { throw 'manifest update failed' }
Write-Host "BUILT: $root\dist\Potflix.jar"
Get-Item "$root\dist\Potflix.jar" | Select Name, Length
