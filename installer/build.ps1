# Builds both executables into bin\ and the installer into dist\.
# Windows PowerShell 5.1: no && / ||, so every native call is followed by an
# explicit exit-code check, and the first failure stops the script.
param([string]$Version = "1.0.0")

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

New-Item -ItemType Directory -Force -Path bin, dist | Out-Null

& go build -ldflags "-s -w" -o bin\devwatt.exe .\cmd\devwatt
if ($LASTEXITCODE -ne 0) { throw "go build devwatt failed with exit code $LASTEXITCODE" }

& go build -ldflags "-s -w -H=windowsgui" -o bin\devwatt-tray.exe .\cmd\devwatt-tray
if ($LASTEXITCODE -ne 0) { throw "go build devwatt-tray failed with exit code $LASTEXITCODE" }

$makensis = Join-Path ${env:ProgramFiles(x86)} 'NSIS\makensis.exe'
if (-not (Test-Path $makensis)) { throw "makensis not found at $makensis" }

& $makensis /V2 "/DVERSION=$Version" installer\devwatt.nsi
if ($LASTEXITCODE -ne 0) { throw "makensis failed with exit code $LASTEXITCODE" }

$out = Get-Item (Join-Path $root "dist\devwatt-setup-$Version.exe")
"{0}  ({1:N2} MB)" -f $out.FullName, ($out.Length / 1MB)
