<#
.SYNOPSIS
  Build a Longarm release on Windows: GoReleaser (all platforms), the MSIs,
  and the flat, minisign-signed directory for `longarm-hub release add`.
.EXAMPLE
  pwsh packaging/build.ps1 -Snapshot                       # local test build, nothing published
  $env:LONGARM_UPDATE_PUBKEY = (Get-Content keys\longarm-release.pub)[-1]
  pwsh packaging/build.ps1 -Key keys\longarm-release.key        # tagged release (GITHUB_TOKEN for publishing)
  pwsh packaging/build.ps1 -SkipPublish                    # tagged release, unsigned, nothing published
                                                           # (CI: .github/workflows/release.yml signs elsewhere)
.NOTES
  Needs: goreleaser v2 (go install github.com/goreleaser/goreleaser/v2@latest),
  WiX v5 for the MSI (packaging/windows/build-msi.ps1), docker only for the
  longarm-hub image (skipped when absent).
#>
param(
  [switch] $Snapshot,
  [string] $Key = $env:LONGARM_RELEASE_KEY,
  [switch] $NoMsi,
  [switch] $SkipPublish,                 # goreleaser --skip=publish,docker: no GitHub release, no token
  # release.yml: "windows" builds only the Windows targets (+ MSIs, no deb/rpm), "unix" only
  # linux + darwin (no MSIs); see LONGARM_BUILD_SIDE in .goreleaser.yaml. Default: everything.
  [ValidateSet('', 'windows', 'unix')] [string] $Side = '',
  [string] $Goreleaser = $env:GORELEASER # goreleaser binary (default: PATH, then GOPATH\bin)
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
  if (Test-Path 'cmd/longarm-tray') { $env:LONGARM_TRAY = '1' } else { Remove-Item Env:LONGARM_TRAY -ErrorAction SilentlyContinue }
  if ($Side) { $env:LONGARM_BUILD_SIDE = $Side } else { Remove-Item Env:LONGARM_BUILD_SIDE -ErrorAction SilentlyContinue }
  if ($Side -eq 'unix') { $NoMsi = $true }
  $gr = $Goreleaser
  if (-not $gr) { $gr = (Get-Command goreleaser -ErrorAction SilentlyContinue).Source }
  if (-not $gr) { $gr = Join-Path (go env GOPATH) 'bin\goreleaser.exe' }
  $grArgs = @('release', '--clean')
  if ($Snapshot) { $grArgs += '--snapshot' }
  $skip = @()
  if ($SkipPublish) { $skip += 'publish' }
  if ($SkipPublish -or -not (Get-Command docker -ErrorAction SilentlyContinue)) { $skip += 'docker' }
  # the Windows side has no linux binaries (nfpm) and no darwin archives (homebrew cask)
  if ($Side -eq 'windows') { $skip += 'nfpm', 'homebrew' }
  if ($skip) { $grArgs += "--skip=$($skip -join ',')" }
  # small CI runners: GoReleaser builds one target per CPU by default and each go build is memory-hungry
  if ($env:LONGARM_GORELEASER_PARALLELISM) { $grArgs += "--parallelism=$($env:LONGARM_GORELEASER_PARALLELISM)" }
  & $gr @grArgs
  if ($LASTEXITCODE -ne 0) { throw "goreleaser failed ($LASTEXITCODE)" }
  $version = (Get-Content dist/metadata.json | ConvertFrom-Json).version
  $out = 'dist/hub-release'
  go run -trimpath ./packaging/hubrelease --dist dist --out $out
  if ($LASTEXITCODE -ne 0) { throw 'hubrelease failed' }
  if (-not $NoMsi) {
    foreach ($a in @(@{go = 'amd64'; wix = 'x64' }, @{go = 'arm64'; wix = 'arm64' })) {
      $bin = Join-Path $env:TEMP "longarm-msi-$($a.go)"
      Remove-Item -Recurse -Force $bin -ErrorAction SilentlyContinue
      New-Item -ItemType Directory $bin | Out-Null
      foreach ($b in 'longarm', 'longarmd', 'longarm-tray') {
        $f = Join-Path $out "$b-windows-$($a.go).exe"
        if (Test-Path $f) { Copy-Item $f (Join-Path $bin "$b.exe") }
      }
      $msi = Join-Path $out "longarm-windows-$($a.go).msi"
      & (Join-Path $PSScriptRoot 'windows/build-msi.ps1') -BinDir $bin -Version $version -Arch $a.wix -Out $msi
      Copy-Item $msi "dist/longarm_$($version)_windows_$($a.go).msi"
    }
  }
  if ($Key) {
    # Sign everything (MSIs included) for the hub; the trusted comment binds name + version.
    go run -trimpath ./packaging/hubrelease --dist dist --out $out --key $Key
    if ($LASTEXITCODE -ne 0) { throw 'signing failed' }
  }
  Write-Host "`nrelease $version"
  Get-ChildItem $out | Format-Table Name, Length -AutoSize
  Write-Host "publish from this machine: longarm admin release publish $out --version $version [--no-sign if signed here]  (or on the hub: longarm-hub release add $out --version $version); then: longarm admin release push $version --canary 1 --wait 30m"
} finally { Pop-Location }
