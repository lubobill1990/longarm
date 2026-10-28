<#
.SYNOPSIS
  CI check of the MSIs build-msi.ps1 made (ci.yml, job msi), without installing them:
  ICE validation (wix msi validate), an administrative install (extracts the files; no
  custom action runs), and the Property / CustomAction / Dialog tables.
.PARAMETER Dir
  Holds default.msi (built with -DefaultHub longarm.example.com) and custom.msi (no built-in hub).
#>
param([Parameter(Mandatory)] [string] $Dir)
$ErrorActionPreference = 'Stop'

function Query([string] $msi, [string] $sql) {
  $inst = New-Object -ComObject WindowsInstaller.Installer
  $db = $inst.GetType().InvokeMember('OpenDatabase', 'InvokeMethod', $null, $inst, @($msi, 0))
  $view = $db.GetType().InvokeMember('OpenView', 'InvokeMethod', $null, $db, @($sql))
  $view.GetType().InvokeMember('Execute', 'InvokeMethod', $null, $view, $null) | Out-Null
  $rows = @()
  while ($true) {
    $rec = $view.GetType().InvokeMember('Fetch', 'InvokeMethod', $null, $view, $null)
    if (-not $rec) { break }
    $rows += $rec.GetType().InvokeMember('StringData', 'GetProperty', $null, $rec, 1)
  }
  $view.GetType().InvokeMember('Close', 'InvokeMethod', $null, $view, $null) | Out-Null
  return , $rows
}
function Prop([string] $msi, [string] $name) {
  $r = Query $msi "SELECT ``Value`` FROM ``Property`` WHERE ``Property`` = '$name'"
  if ($r.Count) { return $r[0] } else { return $null }
}

$default = Join-Path $Dir 'default.msi'
$custom = Join-Path $Dir 'custom.msi'
foreach ($m in $default, $custom) {
  if (-not (Test-Path $m)) { throw "missing $m" }
  Write-Host "== ICE validation: $m"
  wix msi validate $m
  if ($LASTEXITCODE) { throw "wix msi validate $m failed ($LASTEXITCODE)" }
}

Write-Host '== tables'
if ((Prop $default 'DEFAULT_HUB') -ne 'longarm.example.com') { throw "default.msi: DEFAULT_HUB is '$(Prop $default 'DEFAULT_HUB')'" }
if ((Prop $default 'DEFAULT_HUB_CA') -ne ('ab' * 32)) { throw 'default.msi: DEFAULT_HUB_CA missing' }
if ($null -ne (Prop $custom 'DEFAULT_HUB')) { throw 'custom.msi has a DEFAULT_HUB' }
foreach ($p in @{ ROLES = 'client'; LAUNCH_TRAY = '1'; ADMIN_SERVICE_NAME = 'longarmd-admin' }.GetEnumerator()) {
  $v = Prop $default $p.Key
  if ($v -ne $p.Value) { throw "default.msi: $($p.Key) is '$v', want '$($p.Value)'" }
}
# MODE / RUN / ADMIN_WORKER have no default value in the table: SetProperty actions decide
foreach ($p in 'MODE', 'RUN', 'ADMIN_WORKER', 'HUB', 'LOGIN') {
  if ($null -ne (Prop $default $p)) { throw "default.msi: $p has a fixed value (it must stay overridable)" }
}
$secure = (Prop $default 'SecureCustomProperties') -split ';'
foreach ($p in 'MODE', 'RUN', 'ADMIN_WORKER', 'SESSION_SID', 'HUB', 'LOGIN', 'LABELS', 'JOIN_CODE', 'CA_FINGERPRINT') {
  if ($secure -notcontains $p) { throw "default.msi: $p is not a secure property (it would not reach the elevated install)" }
}
$actions = Query $default 'SELECT `Action` FROM `CustomAction`'
foreach ($a in 'LongarmdSetup', 'LongarmdSessionTask', 'LongarmdSessionJoin', 'LongarmdSessionRemove', 'LongarmdAdminSetup',
  'LongarmdAdminRemove', 'LongarmTrayLaunch', 'CheckSessionUser', 'CheckServiceAccount', 'SetModeDefault', 'SetRunDefault') {
  if ($actions -notcontains $a) { throw "default.msi: no custom action $a" }
}
$dialogs = Query $default 'SELECT `Dialog` FROM `Dialog`'
foreach ($d in 'ModeDlg', 'RolesDlg', 'ServiceAccountDlg') {
  if ($dialogs -notcontains $d) { throw "default.msi: no dialog $d" }
}
# the session task's host: the windowless tray
$cmd = Query $default "SELECT ``Target`` FROM ``CustomAction`` WHERE ``Action`` = 'LongarmdSessionTask'"
if ($cmd[0] -notmatch 'longarm-tray\.exe') { throw "LongarmdSessionTask does not run the tray host: $($cmd[0])" }

Write-Host '== administrative install (extract only)'
$target = Join-Path $env:RUNNER_TEMP 'msi-admin'
$log = Join-Path $Dir 'admin.log'
$p = Start-Process msiexec.exe -Wait -PassThru -ArgumentList "/a `"$default`" /qn TARGETDIR=`"$target`" /l*v `"$log`""
if ($p.ExitCode -ne 0) { throw "msiexec /a failed ($($p.ExitCode)); log: $log" }
foreach ($f in 'longarm.exe', 'longarmd.exe', 'longarm-tray.exe') {
  $hit = Get-ChildItem -Recurse -File -Path $target -Filter $f | Select-Object -First 1
  if (-not $hit) { throw "$f not extracted" }
  Write-Host "extracted $($hit.FullName) ($($hit.Length) bytes)"
}
Write-Host 'MSI checks passed'
