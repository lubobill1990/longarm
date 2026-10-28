<#
.SYNOPSIS
  Build the Longarm MSI with WiX v5 (dotnet tool `wix`).
.EXAMPLE
  pwsh packaging/windows/build-msi.ps1 -BinDir dist\win-amd64 -Version 1.2.3 -Arch x64 -Out dist\longarm-windows-amd64.msi
.EXAMPLE
  pwsh packaging/windows/build-msi.ps1 -BinDir dist\win-amd64 -Version 1.2.3 -Out dist\longarm.msi -DefaultHub longarm.example.com
.NOTES
  One-time setup (no admin needed):
    dotnet tool install --global wix --version 5.0.2
    wix extension add -g WixToolset.UI.wixext/5.0.2 WixToolset.Util.wixext/5.0.2 WixToolset.Firewall.wixext/5.0.2
  BinDir holds longarm.exe and longarmd.exe (and longarm-tray.exe, which is included when present;
  longarm-msihelper.exe, the upgrade helper, is built into BinDir with Go when it isn't there).
  The MSI's worker data dir defaults to C:\ProgramData\Longarm\longarmd (DATADIR), the same dir
  longarmd itself uses as LocalSystem and for `service install --isolation per-user`.
  Install modes (longarm.wxs): MODE=default (the built-in hub, -DefaultHub) signs in with Microsoft
  after the install; MODE=custom takes HUB with JOIN_CODE=... (enrolls during the install) or LOGIN=1
  (the bare domain will do: records the hub, so the tray or `longarmd login` signs the machine in
  afterwards with one Microsoft approval and no further input). The worker runs as the signed-in user
  (RUN=session, default) or as a Windows service (RUN=service).
  -DefaultHub: the hub of the "Default" choice (a domain; release.yml passes the repository variable
  LONGARM_DEFAULT_HUB); without it the MSI offers Custom only. -DefaultHubCA optionally pins its CA
  (sha256 hex; LONGARM_DEFAULT_HUB_CA). Build the binaries with the same values (goreleaser reads
  LONGARM_DEFAULT_HUB / LONGARM_DEFAULT_HUB_CA) so that `longarm login` without a hub uses it too.
#>
param(
  [Parameter(Mandatory)] [string] $BinDir,
  [Parameter(Mandatory)] [string] $Version,
  [ValidateSet('x64', 'arm64')] [string] $Arch = 'x64',
  [Parameter(Mandatory)] [string] $Out,
  [string] $DefaultHub = $env:LONGARM_DEFAULT_HUB,
  [string] $DefaultHubCA = $env:LONGARM_DEFAULT_HUB_CA
)
$ErrorActionPreference = 'Stop'
# MSI versions are numeric a.b.c: 1.2.3-rc.1 / 1.2.4-SNAPSHOT-abc -> 1.2.3 / 1.2.4
$msiVersion = ($Version -replace '^v', '') -replace '[-+].*$', ''
if ($msiVersion -notmatch '^\d+\.\d+\.\d+$') { throw "version $Version is not a.b.c" }
foreach ($f in 'longarm.exe', 'longarmd.exe') {
  if (-not (Test-Path (Join-Path $BinDir $f))) { throw "$BinDir has no $f" }
}
# the built-in hub becomes an MSI property and a dialog text: a host name (optionally :port), nothing else
$DefaultHub = "$DefaultHub".Trim() -replace '^https://', '' -replace '/+$', ''
if ($DefaultHub -and $DefaultHub -notmatch '^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:\d{1,5})?$') { throw "-DefaultHub $DefaultHub is not a host name" }
$DefaultHubCA = ("$DefaultHubCA".Trim() -replace '[: ]', '').ToLower()
if ($DefaultHubCA -and $DefaultHubCA -notmatch '^[0-9a-f]{64}$') { throw '-DefaultHubCA is not a sha256 fingerprint (64 hex digits)' }
if ($DefaultHubCA -and -not $DefaultHub) { throw '-DefaultHubCA without -DefaultHub' }
# The upgrade helper (internal/msiupgrade; the MSI's Binary table, not installed): taken from BinDir,
# else built here (small, stdlib + x/sys; needs Go).
$helper = Join-Path (Resolve-Path $BinDir).Path 'longarm-msihelper.exe'
if (-not (Test-Path $helper)) {
  $goArch = @{ x64 = 'amd64'; arm64 = 'arm64' }[$Arch]
  $env:GOOS, $env:GOARCH, $env:CGO_ENABLED = 'windows', $goArch, '0'
  Push-Location (Join-Path $PSScriptRoot '..\..')
  try {
    & go build -trimpath -ldflags '-s -w -H windowsgui' -o $helper ./cmd/longarm-msihelper
    if ($LASTEXITCODE -ne 0) { throw "go build longarm-msihelper failed ($LASTEXITCODE)" }
  } finally {
    Pop-Location
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
  }
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
  -d "DefaultHub=$DefaultHub" -d "DefaultHubCA=$DefaultHubCA" `
  -o $Out $src
if ($LASTEXITCODE -ne 0) { throw "wix build failed ($LASTEXITCODE)" }
Remove-Item ([IO.Path]::ChangeExtension($Out, ".wixpdb")) -ErrorAction SilentlyContinue
Write-Host "built $Out (version $msiVersion, $Arch, tray=$hasTray, default hub: $(if ($DefaultHub) { $DefaultHub } else { 'none: Custom only' }))"
