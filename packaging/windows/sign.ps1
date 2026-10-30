<#
.SYNOPSIS
  Authenticode-sign Windows files with Azure Trusted Signing (Artifact Signing):
  signtool + the Trusted Signing dlib, RFC 3161 timestamp, then a check.
.DESCRIPTION
  Three uses:

    sign.ps1 <file>...        sign (the exes from GoReleaser's build hook, longarm-msihelper.exe,
                              longarm-update.exe and the MSIs from build-msi.ps1). Without
                              LONGARM_SIGN_PROFILE it does nothing: a build is unsigned unless
                              signing was set up, and every caller can call it unconditionally.
    sign.ps1 -Setup <dir>     fetch the dlib (NuGet, pinned by SHA256) into <dir>, find signtool,
                              write <dir>\metadata.json; prints NAME=value lines for the
                              environment (release.yml appends them to GITHUB_ENV). Needs
                              LONGARM_SIGN_ENDPOINT, _ACCOUNT, _PROFILE, or -ToolsOnly (only
                              fetches and checks the tools: release.yml's unsigned builds).
    sign.ps1 -Verify <file>...  every file has a valid Authenticode signature with a timestamp
                              (and, with LONGARM_SIGN_SUBJECT, that publisher).

  Credentials: the dlib uses Azure.Identity's DefaultAzureCredential - in CI the Azure CLI
  login of azure/login (GitHub OIDC); locally `az login`. The identity needs the role
  "Artifact Signing Certificate Profile Signer" (docs/ci-cd.md 2.7).

  Environment (sign):
    LONGARM_SIGN_PROFILE   the certificate profile; empty: don't sign
    LONGARM_SIGNTOOL       signtool.exe (x64)          } written by -Setup
    LONGARM_SIGN_DLIB      Azure.CodeSigning.Dlib.dll  }
    LONGARM_SIGN_METADATA  metadata.json               }
.EXAMPLE
  $env:LONGARM_SIGN_ENDPOINT = 'https://eus.codesigning.azure.net'
  $env:LONGARM_SIGN_ACCOUNT = 'my-account'; $env:LONGARM_SIGN_PROFILE = 'my-profile'
  pwsh packaging/windows/sign.ps1 -Setup $env:TEMP\signing | ForEach-Object { $n, $v = $_ -split '=', 2; Set-Item "Env:$n" $v }
  pwsh packaging/build.ps1 -Snapshot      # GoReleaser's hook and build-msi.ps1 sign as they go
#>
[CmdletBinding(DefaultParameterSetName = 'Sign')]
param(
  [Parameter(ParameterSetName = 'Sign', Position = 0, ValueFromRemainingArguments)]
  [Parameter(ParameterSetName = 'Verify', Position = 0, ValueFromRemainingArguments)]
  [string[]] $Path,
  [Parameter(ParameterSetName = 'Setup', Mandatory)] [string] $Setup,
  [Parameter(ParameterSetName = 'Setup')] [switch] $ToolsOnly,
  [Parameter(ParameterSetName = 'Verify', Mandatory)] [switch] $Verify
)
$ErrorActionPreference = 'Stop'

# Microsoft.ArtifactSigning.Client (the Trusted Signing dlib for signtool), from nuget.org
$DlibVersion = '1.0.146'
$DlibSha256 = '7761e5d8f31e54b764da21d33cd8b0a2940e1b05526c7dfcb5e94b0431426f70'
$Timestamp = 'http://timestamp.acs.microsoft.com'
$Description = 'Longarm'
$Url = 'https://github.com/lubobill1990/longarm'

function Find-SignTool {
  $kits = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10\bin'
  $st = Get-ChildItem $kits -Filter signtool.exe -Recurse -ErrorAction SilentlyContinue |
    Where-Object { $_.Directory.Name -eq 'x64' -and $_.Directory.Parent.Name -match '^10\.' } |
    Sort-Object { [version]$_.Directory.Parent.Name } | Select-Object -Last 1
  if (-not $st) { throw "signtool.exe (x64) not found under $kits (Windows SDK)" }
  $st.FullName
}

if ($PSCmdlet.ParameterSetName -eq 'Setup') {
  New-Item -ItemType Directory -Force $Setup | Out-Null
  $dir = (Resolve-Path $Setup).Path
  $pkg = Join-Path $dir "microsoft.artifactsigning.client.$DlibVersion.nupkg"
  if (-not (Test-Path $pkg)) {
    Invoke-WebRequest -UseBasicParsing -OutFile $pkg -Uri "https://www.nuget.org/api/v2/package/Microsoft.ArtifactSigning.Client/$DlibVersion"
  }
  $sha = (Get-FileHash -Algorithm SHA256 $pkg).Hash.ToLower()
  if ($sha -ne $DlibSha256) { Remove-Item $pkg; throw "Microsoft.ArtifactSigning.Client $DlibVersion sha256 mismatch: $sha" }
  $lib = Join-Path $dir 'dlib'
  Remove-Item -Recurse -Force $lib -ErrorAction SilentlyContinue
  Expand-Archive -Force $pkg $lib  # a .nupkg is a zip
  $dlib = Join-Path $lib 'bin\x64\Azure.CodeSigning.Dlib.dll'
  if (-not (Test-Path $dlib)) { throw "no bin\x64\Azure.CodeSigning.Dlib.dll in the package" }
  $signtool = Find-SignTool
  # the dlib runs on .NET 8 (release.yml installs it for WiX anyway)
  if (-not (& dotnet --list-runtimes 2>$null | Select-String '^Microsoft\.NETCore\.App (8|9|1\d)\.')) { throw 'the dlib needs the .NET 8+ runtime' }
  [Console]::Error.WriteLine("dlib $DlibVersion (sha256 ok), signtool $signtool")
  if ($ToolsOnly) { return }
  $endpoint = "$env:LONGARM_SIGN_ENDPOINT".Trim().TrimEnd('/')
  $account = "$env:LONGARM_SIGN_ACCOUNT".Trim()
  $profileName = "$env:LONGARM_SIGN_PROFILE".Trim()
  if (-not ($endpoint -and $account -and $profileName)) { throw 'LONGARM_SIGN_ENDPOINT, LONGARM_SIGN_ACCOUNT and LONGARM_SIGN_PROFILE are required' }
  # the dlib accepts only these; a typo here would otherwise surface as a 403 at signing time
  if ($endpoint -notmatch '^https://[a-z0-9]+\.codesigning\.azure\.net$') { throw "LONGARM_SIGN_ENDPOINT $endpoint is not https://<region>.codesigning.azure.net" }
  if ($account -notmatch '^[A-Za-z][A-Za-z0-9-]{1,22}[A-Za-z0-9]$') { throw "LONGARM_SIGN_ACCOUNT $account is not an account name" }
  if ($profileName -notmatch '^[A-Za-z][A-Za-z0-9-]{3,98}[A-Za-z0-9]$') { throw "LONGARM_SIGN_PROFILE $profileName is not a certificate profile name" }
  $meta = Join-Path $dir 'metadata.json'
  [ordered]@{
    Endpoint               = $endpoint
    CodeSigningAccountName = $account
    CertificateProfileName = $profileName
    # GitHub runners have no managed identity: skip its slow probe (the Azure CLI login is used)
    ExcludeCredentials     = @('ManagedIdentityCredential', 'WorkloadIdentityCredential', 'SharedTokenCacheCredential',
      'VisualStudioCredential', 'VisualStudioCodeCredential', 'InteractiveBrowserCredential')
  } | ConvertTo-Json | Set-Content -Encoding utf8NoBOM $meta
  "LONGARM_SIGNTOOL=$signtool"
  "LONGARM_SIGN_DLIB=$dlib"
  "LONGARM_SIGN_METADATA=$meta"
  return
}

$files = @($Path | Where-Object { $_ } | ForEach-Object { (Resolve-Path $_).Path })
if (-not $files) { throw 'no files' }

if ($Verify) {
  $want = "$env:LONGARM_SIGN_SUBJECT".Trim()
  $bad = 0
  foreach ($f in $files) {
    $s = Get-AuthenticodeSignature -LiteralPath $f
    $subject = if ($s.SignerCertificate) { $s.SignerCertificate.Subject } else { '' }
    $why = @()
    if ($s.Status -ne 'Valid') { $why += "status $($s.Status): $($s.StatusMessage)" }
    if (-not $s.TimeStamperCertificate) { $why += 'no timestamp' }
    if ($want -and $subject -notmatch "(^|, )CN=$([regex]::Escape($want))(,|$)") { $why += "signer $subject, not CN=$want" }
    if ($why) { Write-Host "::error file=$(Split-Path -Leaf $f)::$(Split-Path -Leaf $f): $($why -join '; ')"; $bad++ }
    else { Write-Host "ok  $(Split-Path -Leaf $f)  $subject  (timestamp $($s.TimeStamperCertificate.Subject))" }
  }
  if ($bad) { throw "$bad of $($files.Count) files are not properly signed" }
  return
}

if (-not "$env:LONGARM_SIGN_PROFILE".Trim()) { return } # signing not set up: unsigned build
foreach ($v in 'LONGARM_SIGNTOOL', 'LONGARM_SIGN_DLIB', 'LONGARM_SIGN_METADATA') {
  if (-not (Test-Path -LiteralPath "$((Get-Item "Env:$v" -ErrorAction SilentlyContinue).Value)")) { throw "$v is not set (run sign.ps1 -Setup first)" }
}
$st = $env:LONGARM_SIGNTOOL
$stArgs = @('sign', '/v', '/fd', 'SHA256', '/tr', $Timestamp, '/td', 'SHA256', '/d', $Description, '/du', $Url,
  '/dlib', $env:LONGARM_SIGN_DLIB, '/dmdf', $env:LONGARM_SIGN_METADATA) + $files
# the service and the timestamp server are remote: retry transient failures
for ($try = 1; ; $try++) {
  & $st @stArgs
  if ($LASTEXITCODE -eq 0) { break }
  if ($try -ge 3) { throw "signtool sign failed ($LASTEXITCODE): $($files -join ', ')" }
  Write-Warning "signtool sign failed ($LASTEXITCODE), retrying in $(10 * $try) s"
  Start-Sleep -Seconds (10 * $try)
}
& $PSCommandPath -Verify @files
