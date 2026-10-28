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
foreach ($p in @{ ROLES = 'client'; LAUNCH_TRAY = '1' }.GetEnumerator()) {
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
foreach ($a in 'LongarmdCheck', 'LongarmdSettings', 'LongarmdSettingsRemove', 'LongarmdSetup', 'LongarmdSessionTask', 'LongarmdSessionJoin', 'LongarmdSessionRemove', 'LongarmdAdminSetup',
  'LongarmdAdminRemove', 'LongarmTrayLaunch', 'CheckSessionUser', 'CheckServiceAccount', 'SetModeDefault', 'SetRunDefault') {
  if ($actions -notcontains $a) { throw "default.msi: no custom action $a" }
}
$dialogs = Query $default 'SELECT `Dialog` FROM `Dialog`'
foreach ($d in 'ModeDlg', 'RolesDlg', 'ServiceAccountDlg') {
  if ($dialogs -notcontains $d) { throw "default.msi: no dialog $d" }
}
# every install-helper custom action passes its values as data after the marker (parsed strictly by the
# helper), never as options, and names no program or path of its own (security.md "Installer")
# (MSI SQL has no LIKE: filter here)
$all = Query $default 'SELECT `Target` FROM `CustomAction`'
$targets = @($all | Where-Object { $_ -like '*install-helper*' })
if ($targets.Count -lt 10) { throw "only $($targets.Count) install-helper custom actions" }
foreach ($t in $targets) {
  if ($t -notmatch 'install-helper msi [a-z-]+ #LONGARM-MSI#') { throw "install-helper action without structured data: $t" }
  $data = $t.Substring($t.IndexOf('#LONGARM-MSI#'))
  if ($data -match ' --|--exe|--host|--data-dir|TempFolder|\[INSTALLFOLDER\]') { throw "install-helper data names an option or a path: $t" }
}
# the sign-in defaults reach HKLM through the helper only (validated, admin-checked), not the Registry table
$regNames = Query $default 'SELECT `Name` FROM `Registry`'
foreach ($n in 'Hub', 'CaFingerprint', 'Login', 'Labels', 'Mode') {
  if ($regNames -contains $n) { throw "the Registry table writes $n from a property" }
}
# role choices: the admin worker only with a worker role, the Cloud PC / Dev Box preselection
# (AppSearch, UI sequence only), the Finish text
foreach ($a in 'CheckAdminRole', 'SetCLOUD_PC', 'SetDefaultRolesCloud', 'SetRolesCloud', 'SetFinishText', 'SetFinishAdmin') {
  if ($actions -notcontains $a) { throw "default.msi: no custom action $a" }
}
$ui = Query $default 'SELECT `Action` FROM `InstallUISequence`'
$exec = Query $default 'SELECT `Action` FROM `InstallExecuteSequence`'
foreach ($a in 'SetCLOUD_PC', 'SetDefaultRolesCloud', 'SetRolesCloud', 'SetFinishText') {
  if ($ui -notcontains $a) { throw "default.msi: $a is not in the UI sequence" }
  if ($exec -contains $a) { throw "default.msi: $a runs in the execute sequence (a silent install must keep ROLES)" }
}
if ($exec -notcontains 'CheckAdminRole') { throw 'default.msi: CheckAdminRole is not scheduled' }
$searches = Query $default 'SELECT `Property` FROM `AppSearch`'
foreach ($p in 'DEVBOX_AGENT', 'DEVBOX_AGENT_SVC', 'ROLES_RECORDED') {
  if ($searches -notcontains $p) { throw "default.msi: no AppSearch for $p" }
}
$cloud = Query $default "SELECT ``Target`` FROM ``CustomAction`` WHERE ``Action`` = 'SetCLOUD_PC'"
if (-not $cloud.Count) { throw 'default.msi: SetCLOUD_PC has no value' }
$cloudCond = Query $default "SELECT ``Condition`` FROM ``InstallUISequence`` WHERE ``Action`` = 'SetCLOUD_PC'"
if ($cloudCond[0] -notlike '*ComputerName ~<< "CPC-"*' -or $cloudCond[0] -notlike '*DEVBOX_AGENT*') { throw "default.msi: SetCLOUD_PC condition '$($cloudCond[0])'" }
$adminDisable = Query $default "SELECT ``Condition`` FROM ``ControlCondition`` WHERE ``Dialog_`` = 'ModeDlg' AND ``Control_`` = 'Admin' AND ``Action`` = 'Disable'"
if ($adminDisable[0] -notlike '*DEFAULT_ROLES="client"*') { throw "default.msi: ModeDlg's admin checkbox is enabled for a client ('$($adminDisable[0])')" }
$adminDisable = Query $default "SELECT ``Condition`` FROM ``ControlCondition`` WHERE ``Dialog_`` = 'RolesDlg' AND ``Control_`` = 'Admin' AND ``Action`` = 'Disable'"
if ($adminDisable[0] -notlike '*ROLES="client"*') { throw "default.msi: RolesDlg's admin checkbox is enabled for a client ('$($adminDisable[0])')" }
$texts = Query $default "SELECT ``Text`` FROM ``RadioButton`` WHERE ``Property`` = 'DEFAULT_ROLES'"
foreach ($t in 'This is my &everyday computer: send jobs to workers', 'This &machine runs jobs (Dev Box, build machine)') {
  if ($texts -notcontains $t) { throw "default.msi: ModeDlg has no '$t'" }
}
foreach ($p in 'ADMIN_SERVICE_NAME', 'ADMIN_DATADIR', 'ADMIN_HANDOFF') {
  if ($secure -contains $p) { throw "$p must not be a (secure) property any more" }
}

# upgrades with Longarm running (internal/msiupgrade, docs/usage.md "Upgrading"): no Restart Manager, the
# upgrade helper from the Binary table, its steps in the right places, the services stopped and waited for
function Seq([string] $action) {
  $r = Query $default "SELECT ``Sequence`` FROM ``InstallExecuteSequence`` WHERE ``Action`` = '$action'"
  if (-not $r.Count) { throw "default.msi: $action is not in InstallExecuteSequence" }
  return [int]$r[0]
}
if ((Prop $default 'MSIRESTARTMANAGERCONTROL') -ne 'Disable') { throw 'default.msi: MSIRESTARTMANAGERCONTROL is not Disable' }
$binaries = Query $default 'SELECT `Name` FROM `Binary`'
if ($binaries -notcontains 'LongarmMsiHelper') { throw 'default.msi: no LongarmMsiHelper in the Binary table' }
foreach ($a in @{ LongarmStopTray = 'stop-tray'; LongarmUpgradeStop = 'stop'; LongarmUpgradeStopRollback = 'rollback'; LongarmRemoveStop = 'stop-remove' }.GetEnumerator()) {
  $src = Query $default "SELECT ``Source`` FROM ``CustomAction`` WHERE ``Action`` = '$($a.Key)'"
  $tgt = Query $default "SELECT ``Target`` FROM ``CustomAction`` WHERE ``Action`` = '$($a.Key)'"
  if ($src[0] -ne 'LongarmMsiHelper') { throw "default.msi: $($a.Key) does not run the upgrade helper ($($src[0]))" }
  if ($tgt[0] -ne "$($a.Value) #LONGARM-MSI#usersid=[UserSID]") { throw "default.msi: $($a.Key) target '$($tgt[0])'" }
}
$order = @(
  @('LongarmStopTray', 'InstallValidate'),
  @('InstallInitialize', 'LongarmUpgradeStopRollback'),
  @('LongarmUpgradeStopRollback', 'LongarmUpgradeStop'),
  @('LongarmUpgradeStop', 'RemoveExistingProducts'),
  @('RemoveExistingProducts', 'InstallFiles'),
  @('LongarmRemoveStop', 'StopServices'),
  @('InstallInitialize', 'LongarmRemoveStop'),
  @('LongarmTrayLaunch', 'LongarmdUpgradeRestart'),
  @('LongarmdAdminSetup', 'LongarmdUpgradeRestart'),
  @('StartServices', 'LongarmdUpgradeRestart'),
  @('LongarmdUpgradeRestart', 'InstallFinalize')
)
foreach ($o in $order) {
  $a, $b = (Seq $o[0]), (Seq $o[1])
  if ($a -ge $b) { throw "default.msi: $($o[0]) ($a) must run before $($o[1]) ($b)" }
  Write-Host "$($o[0]) ($a) < $($o[1]) ($b)"
}
foreach ($id in 'AdminSvcStop', 'LongarmdSvcCtl') {
  $ev = Query $default "SELECT ``Event`` FROM ``ServiceControl`` WHERE ``ServiceControl`` = '$id'"
  $wait = Query $default "SELECT ``Wait`` FROM ``ServiceControl`` WHERE ``ServiceControl`` = '$id'"
  if (-not $ev.Count -or ([int]$ev[0] -band 0x22) -ne 0x22 -or $wait[0] -ne '1') { throw "default.msi: ServiceControl $id must stop on install and uninstall and wait (Event $($ev[0]), Wait $($wait[0]))" }
}

# the Longarm Update service (internal/updatesvc): every install, LocalSystem, auto start (delayed),
# "run"; stopped and waited for before its file changes, removed on uninstall; started without waiting
$svc = Query $default "SELECT ``Name`` FROM ``ServiceInstall`` WHERE ``ServiceInstall`` = 'UpdateSvc'"
if ($svc[0] -ne 'LongarmUpdate') { throw "default.msi: no ServiceInstall LongarmUpdate ($svc)" }
$acct = Query $default "SELECT ``StartName`` FROM ``ServiceInstall`` WHERE ``ServiceInstall`` = 'UpdateSvc'"
$args_ = Query $default "SELECT ``Arguments`` FROM ``ServiceInstall`` WHERE ``ServiceInstall`` = 'UpdateSvc'"
$start = Query $default "SELECT ``StartType`` FROM ``ServiceInstall`` WHERE ``ServiceInstall`` = 'UpdateSvc'"
$comp = Query $default "SELECT ``Component_`` FROM ``ServiceInstall`` WHERE ``ServiceInstall`` = 'UpdateSvc'"
if ($acct[0] -ne 'LocalSystem' -or $args_[0] -ne 'run' -or [int]$start[0] -ne 2 -or $comp[0] -ne 'UpdateService') {
  throw "default.msi: LongarmUpdate account '$($acct[0])', arguments '$($args_[0])', start $($start[0]), component $($comp[0])"
}
$cond = Query $default "SELECT ``Condition`` FROM ``Component`` WHERE ``Component`` = 'UpdateService'"
if ($cond.Count -and $cond[0]) { throw "default.msi: the update service is conditional ($($cond[0])): every install has it" }
$ev = Query $default "SELECT ``Event`` FROM ``ServiceControl`` WHERE ``ServiceControl`` = 'UpdateSvcStop'"
$wait = Query $default "SELECT ``Wait`` FROM ``ServiceControl`` WHERE ``ServiceControl`` = 'UpdateSvcStop'"
if (-not $ev.Count -or ([int]$ev[0] -band 0xA2) -ne 0xA2 -or $wait[0] -ne '1') { throw "default.msi: UpdateSvcStop must stop on install and uninstall, remove on uninstall and wait (Event $($ev[0]), Wait $($wait[0]))" }
$ev = Query $default "SELECT ``Event`` FROM ``ServiceControl`` WHERE ``ServiceControl`` = 'UpdateSvcStart'"
$wait = Query $default "SELECT ``Wait`` FROM ``ServiceControl`` WHERE ``ServiceControl`` = 'UpdateSvcStart'"
if (-not $ev.Count -or ([int]$ev[0] -band 0x1) -ne 0x1 -or $wait[0] -eq '1') { throw "default.msi: UpdateSvcStart must start on install without waiting (Event $($ev[0]), Wait $($wait[0]))" }
$files = Query $default 'SELECT `FileName` FROM `File`'
if (-not ($files | Where-Object { $_ -like '*longarm-update.exe' })) { throw 'default.msi: no longarm-update.exe in the File table' }
Write-Host "LongarmUpdate: LocalSystem, auto start, run"

Write-Host '== administrative install (extract only)'
$target = Join-Path $env:RUNNER_TEMP 'msi-admin'
$log = Join-Path $Dir 'admin.log'
$p = Start-Process msiexec.exe -Wait -PassThru -ArgumentList "/a `"$default`" /qn TARGETDIR=`"$target`" /l*v `"$log`""
if ($p.ExitCode -ne 0) { throw "msiexec /a failed ($($p.ExitCode)); log: $log" }
foreach ($f in 'longarm.exe', 'longarmd.exe', 'longarm-tray.exe', 'longarm-update.exe') {
  $hit = Get-ChildItem -Recurse -File -Path $target -Filter $f | Select-Object -First 1
  if (-not $hit) { throw "$f not extracted" }
  Write-Host "extracted $($hit.FullName) ($($hit.Length) bytes)"
}
Write-Host 'MSI checks passed'
