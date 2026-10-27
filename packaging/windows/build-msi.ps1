<#
.SYNOPSIS
  Build the Longarm MSI with WiX v5 (dotnet tool `wix`).
.EXAMPLE
  pwsh packaging/windows/build-msi.ps1 -BinDir dist\win-amd64 -Version 1.2.3 -Arch x64 -Out dist\longarm-windows-amd64.msi
.NOTES
  One-time setup (no admin needed):
    dotnet tool install --global wix --version 5.0.2
    wix extension add -g WixToolset.UI.wixext/5.0.2 WixToolset.Util.wixext/5.0.2 WixToolset.Firewall.wixext/5.0.2
  BinDir holds longarm.exe and longarmd.exe (and longarm-tray.exe, which is included when present).
  The MSI's worker data dir defaults to C:\ProgramData\Longarm\longarmd (DATADIR), the same dir
  longarmd itself uses as LocalSystem and for `service install --isolation per-user`.
  Install modes (longarm.wxs): JOIN_CODE=... enrolls during the install; LOGIN=1 (with HUB, the bare
  domain will do) installs without a code and records the hub, so the tray or `longarmd login`
  signs the machine in afterwards with one Microsoft approval and no further input.
#>
param(
  [Parameter(Mandatory)] [string] $BinDir,
  [Parameter(Mandatory)] [string] $Version,
  [ValidateSet('x64', 'arm64')] [string] $Arch = 'x64',
  [Parameter(Mandatory)] [string] $Out
)
$ErrorActionPreference = 'Stop'
# MSI versions are numeric a.b.c: 1.2.3-rc.1 / 1.2.4-SNAPSHOT-abc -> 1.2.3 / 1.2.4
$msiVersion = ($Version -replace '^v', '') -replace '[-+].*$', ''
if ($msiVersion -notmatch '^\d+\.\d+\.\d+$') { throw "version $Version is not a.b.c" }
foreach ($f in 'longarm.exe', 'longarmd.exe') {
  if (-not (Test-Path (Join-Path $BinDir $f))) { throw "$BinDir has no $f" }
}
$hasTray = if (Test-Path (Join-Path $BinDir 'longarm-tray.exe')) { 1 } else { 0 }
$wix = Get-Command wix -ErrorAction SilentlyContinue
if (-not $wix) {
  $cand = Join-Path $env:USERPROFILE '.dotnet\tools\wix.exe'
  if (Test-Path $cand) { $wix = $cand } else { throw 'wix not found (dotnet tool install --global wix --version 5.0.2)' }
}
if (-not $env:DOTNET_ROOT -and (Test-Path (Join-Path $env:USERPROFILE '.dotnet\dotnet.exe'))) {
  $env:DOTNET_ROOT = Join-Path $env:USERPROFILE '.dotnet'
}
$src = Join-Path $PSScriptRoot 'longarm.wxs'
New-Item -ItemType Directory -Force (Split-Path -Parent ([IO.Path]::GetFullPath($Out))) | Out-Null
& $wix build -nologo -arch $Arch `
  -ext WixToolset.UI.wixext -ext WixToolset.Util.wixext -ext WixToolset.Firewall.wixext `
  -d "Version=$msiVersion" -d "BinDir=$((Resolve-Path $BinDir).Path)" -d "HasTray=$hasTray" `
  -o $Out $src
if ($LASTEXITCODE -ne 0) { throw "wix build failed ($LASTEXITCODE)" }
Remove-Item ([IO.Path]::ChangeExtension($Out, ".wixpdb")) -ErrorAction SilentlyContinue
Write-Host "built $Out (version $msiVersion, $Arch, tray=$hasTray)"
