<#
.SYNOPSIS
  Render the winget manifests for a release (then `winget validate` and
  submit to a private source or microsoft/winget-pkgs).
.EXAMPLE
  pwsh packaging/winget/render.ps1 -Version 1.2.3 -BaseUrl https://github.com/lubobill1990/longarm/releases/download/v1.2.3 -Dist dist
#>
param(
  [Parameter(Mandatory)] [string] $Version,
  [Parameter(Mandatory)] [string] $BaseUrl,
  [string] $Dist = 'dist',
  [string] $Out = 'dist/winget'
)
$ErrorActionPreference = 'Stop'
$x64 = "longarm_$($Version)_windows_amd64.msi"
$arm = "longarm_$($Version)_windows_arm64.msi"
$vals = @{
  '{{VERSION}}'      = $Version
  '{{URL_X64}}'      = "$BaseUrl/$x64"
  '{{SHA256_X64}}'   = (Get-FileHash (Join-Path $Dist $x64) -Algorithm SHA256).Hash
  '{{URL_ARM64}}'    = "$BaseUrl/$arm"
  '{{SHA256_ARM64}}' = (Get-FileHash (Join-Path $Dist $arm) -Algorithm SHA256).Hash
}
New-Item -ItemType Directory -Force $Out | Out-Null
Get-ChildItem (Join-Path $PSScriptRoot 'manifests') -Filter *.tmpl | ForEach-Object {
  $t = Get-Content $_.FullName -Raw
  foreach ($k in $vals.Keys) { $t = $t.Replace($k, $vals[$k]) }
  Set-Content -NoNewline -Path (Join-Path $Out ($_.Name -replace '\.tmpl$', '')) -Value $t
}
Write-Host "winget manifests in $Out (winget validate $Out)"
