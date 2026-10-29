<#
.SYNOPSIS
  CI only (ci.yml, job msi-install; a disposable windows-latest runner, as its administrator): really installs,
  repairs, upgrades and uninstalls Longarm MSIs. NEVER run this on your own machine: it installs and removes
  Longarm, its services and scheduled tasks, and ends Longarm's processes.
.DESCRIPTION
  Scenarios (-Scenario):
    fresh          the PR's MSI (Default mode, ROLES=both: the user-session worker of the runner's user, LOGIN=1
                   with the build's dummy hub - nobody signs in), a repair, an upgrade that would switch to a
                   service (refused, nothing changes), an upgrade to the PR's next build with the tray and the
                   worker running, the uninstall
    upgrade        -OldMsi (a released version) installed with -OldArgs, the tray and the worker running, then
                   the PR's MSI over it, then the uninstall
  Every msiexec run is logged verbosely and the log is checked: no error 2613 / 1722 / 1603, no failed
  custom action, no file held in use (a reboot), exit code 0. An upgrade must stop every Longarm process
  that ran (tray, worker host, longarmd run, the services) and start each kind again (new processes), leave
  a running job's console host (longarmd conhost -- ping ..., what the worker runs a job's command in) and
  its command running (longarmd.exe moved aside),
  leave one Longarm product of the new version, the new files (sha256 as built), one firewall rule each.
.PARAMETER MsiDir
  The msi job's artifact: default.msi (version 0.250.0), next.msi (0.250.1), hashes-default.txt, hashes-next.txt.
#>
param(
  [Parameter(Mandatory)] [ValidateSet('fresh', 'upgrade')] [string] $Scenario,
  [Parameter(Mandatory)] [string] $MsiDir,
  [string] $OldMsi,
  [string] $OldArgs = '',
  [string] $LogDir = (Join-Path $env:RUNNER_TEMP 'msi-logs')
)
$ErrorActionPreference = 'Stop'
if (-not $env:GITHUB_ACTIONS) { throw 'CI only: this installs and removes Longarm (see the header)' }
New-Item -ItemType Directory -Force $LogDir | Out-Null
$installDir = Join-Path ${env:ProgramFiles} 'Longarm'
$dataDir = Join-Path $env:ProgramData 'Longarm'
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$script:step = 0
Write-Host "user $([Environment]::UserName) ($sid), session $((Get-Process -Id $PID).SessionId)"

function Fail([string] $msg) {
  Write-Host "::error::$msg"
  Show-Diagnostics
  throw $msg
}

function Show-Diagnostics {
  Write-Host '--- Longarm processes'
  Get-LongarmProcs | Format-Table -AutoSize ProcessId, Kind, SessionId, CommandLine | Out-String -Width 300 | Write-Host
  foreach ($f in 'msi-upgrade.json', 'install-helper.log') {
    $p = Join-Path $dataDir $f
    if (Test-Path $p) { Write-Host "--- $p"; Get-Content $p -Tail 80 | Write-Host }
  }
  Get-ScheduledTask -TaskName 'Longarm*' -ErrorAction SilentlyContinue | Format-Table -AutoSize TaskName, State | Out-String | Write-Host
  Get-Service -Name 'Longarm*', 'longarmd*' -ErrorAction SilentlyContinue | Format-Table -AutoSize Name, Status, StartType | Out-String | Write-Host
}

# --- state ---------------------------------------------------------------------------------------------

function Get-LongarmProcs {
  $prefix = $installDir.TrimEnd('\') + '\'
  Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith($prefix, 'OrdinalIgnoreCase') } | ForEach-Object {
    $name = $_.Name.ToLower()
    $cmd = "$($_.CommandLine)"
    # (a job's console host whose longarmd.exe was moved aside runs as old\longarmd-<n>.exe)
    $kind = if ($cmd -match '\sconhost\s' -and $name -match '^longarmd') { 'job' } else {
      switch -Regex ($name) {
        '^longarm-tray\.exe$' { if ($cmd -match '--?worker-host') { 'host' } else { 'tray' } }
        '^longarmd\.exe$' { if ($cmd -match '\ssession-mode\s+run\b') { 'host' } elseif ($cmd -match '\srun\b') { 'daemon' } else { 'longarmd-other' } }
        '^longarm-update\.exe$' { 'update' }
        default { 'other' }
      }
    }
    [pscustomobject]@{ ProcessId = $_.ProcessId; Kind = $kind; Name = $name; SessionId = $_.SessionId; CommandLine = $cmd; Exe = $_.ExecutablePath }
  }
}

function Get-Products {
  @(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*', 'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*' -ErrorAction SilentlyContinue |
      Where-Object { $_.DisplayName -eq 'Longarm' })
}

function Wait-Until([scriptblock] $cond, [int] $seconds, [string] $what) {
  $deadline = (Get-Date).AddSeconds($seconds)
  while ((Get-Date) -lt $deadline) {
    if (& $cond) { return $true }
    Start-Sleep -Seconds 2
  }
  Write-Host "timed out after ${seconds}s waiting for $what"
  return $false
}

# the kinds an upgrade stops and starts again (a job's processes are left running)
$restartKinds = 'tray', 'host', 'daemon'

function Start-Running([bool] $wantTray, [bool] $wantHost) {
  # the install started them (LongarmTrayLaunch, the worker task); start what isn't running, as a user would
  if ($wantHost -and -not (Wait-Until { @(Get-LongarmProcs | Where-Object Kind -EQ 'host').Count } 60 'the worker host')) {
    Write-Host "starting LongarmWorker-$sid"
    Start-ScheduledTask -TaskName "LongarmWorker-$sid"
    if (-not (Wait-Until { @(Get-LongarmProcs | Where-Object Kind -EQ 'host').Count } 60 'the worker host')) { Fail 'the user-session worker does not run' }
  }
  if ($wantTray -and -not (Wait-Until { @(Get-LongarmProcs | Where-Object Kind -EQ 'tray').Count } 30 'the tray')) {
    Write-Host 'starting the tray'
    Start-Process (Join-Path $installDir 'longarm-tray.exe')
    if (-not (Wait-Until { @(Get-LongarmProcs | Where-Object Kind -EQ 'tray').Count } 30 'the tray')) { Fail 'the tray does not run' }
  }
  # a running job: its console host (longarmd conhost, as the worker starts a job's command in) and the
  # command, which an upgrade must leave running, longarmd.exe moved aside
  if (-not @(Get-LongarmProcs | Where-Object Kind -EQ 'job').Count) {
    Start-Process -WindowStyle Hidden (Join-Path $installDir 'longarmd.exe') -ArgumentList 'conhost', '--', 'ping.exe', '-n', '1800', '127.0.0.1'
    if (-not (Wait-Until { @(Get-LongarmProcs | Where-Object Kind -EQ 'job').Count } 20 'the job')) { Fail 'longarmd conhost does not run' }
  }
  Start-Sleep -Seconds 5 # let them settle (a host starts its longarmd)
  Get-LongarmProcs | Format-Table -AutoSize ProcessId, Kind, SessionId, CommandLine | Out-String -Width 300 | Write-Host
}

function Get-ServicePids {
  $m = @{}
  foreach ($s in Get-CimInstance Win32_Service | Where-Object { $_.Name -in 'LongarmUpdate', 'longarmd', 'longarmd-admin' -and $_.State -eq 'Running' }) {
    $m[$s.Name] = [int]$s.ProcessId
  }
  return $m
}

# --- msiexec ---------------------------------------------------------------------------------------------

function Invoke-Msi([string] $what, [string] $arguments, [int[]] $ok = @(0), [string[]] $allow = @()) {
  $script:step++
  $log = Join-Path $LogDir ('{0:d2}-{1}.log' -f $script:step, ($what -replace '[^A-Za-z0-9.-]+', '-'))
  Write-Host "== $what`: msiexec $arguments"
  $t = Get-Date
  $p = Start-Process msiexec.exe -Wait -PassThru -ArgumentList "$arguments /qn /norestart /l*v `"$log`""
  Write-Host "exit $($p.ExitCode) after $([int]((Get-Date) - $t).TotalSeconds)s; log $log"
  $text = Get-Content $log -Raw # UTF-16 with a BOM, or ANSI
  $bad = @()
  # InstallValidate lists the processes holding files ("Info 1603. The file ... is being held in use") before
  # the elevated stop step ends them: informational, silent installs show no dialog. A file still in use when
  # InstallFiles replaces it is a reboot: ReplacedInUseFiles, exit code 3010.
  foreach ($m in [regex]::Matches($text, '(?m)^.*is being held in use.*$')) { Write-Host "  (InstallValidate) $($m.Value.Trim())" }
  foreach ($pat in '\b(?:Error|Note: 1: |error code )(2613|1722|1603)\b', 'Return value 3\.', 'ReplacedInUseFiles = 1',
    'returned actual error code','MainEngineThread is returning (?!0\b)\d+', 'Product: Longarm -- (?:Installation|Removal|Configuration) failed') {
    foreach ($m in [regex]::Matches($text, "(?m)^.*$pat.*$")) {
      $line = $m.Value.Trim()
      if (@($allow | Where-Object { $line -match $_ }).Count) { Write-Host "allowed: $line"; continue }
      $bad += $line
    }
  }
  foreach ($m in [regex]::Matches($text, '(?m)^.*Disallowing uninstallation of component.*$')) { Write-Host "  $($m.Value.Trim())" }
  if ($ok -notcontains $p.ExitCode) { $bad += "msiexec exit code $($p.ExitCode) (want $($ok -join ' or '))" }
  if ($bad.Count) {
    Write-Host ($text -split "`n" | Select-String -Pattern 'Return value 3' -Context 30, 2 | Select-Object -First 2 | Out-String)
    Fail ("$what`: " + (($bad | Select-Object -Unique -First 12) -join "`n  "))
  }
  return $text
}

# --- checks ----------------------------------------------------------------------------------------------

function Assert-Installed([string] $version, [string] $hashes) {
  $prods = Get-Products
  if ($prods.Count -ne 1) { Fail "want one Longarm product, have $($prods.Count): $($prods.DisplayVersion -join ', ')" }
  if ($prods[0].DisplayVersion -ne $version) { Fail "installed version $($prods[0].DisplayVersion), want $version" }
  foreach ($line in Get-Content $hashes) {
    $name, $want = $line -split '\s+'
    $f = Join-Path $installDir $name
    if (-not (Test-Path $f)) { Fail "$f is missing" }
    $got = (Get-FileHash -Algorithm SHA256 $f).Hash.ToLower()
    if ($got -ne $want.ToLower()) { Fail "$f is not the $version build ($got, want $want)" }
  }
  foreach ($rule in 'Longarm longarm (UDP)', 'Longarm longarmd (TCP)') {
    $n = @(Get-NetFirewallRule -DisplayName $rule -ErrorAction SilentlyContinue).Count
    if ($n -ne 1) { Fail "firewall rule '$rule': $n of them" }
  }
  if (-not (Get-Service LongarmUpdate -ErrorAction SilentlyContinue)) { Fail 'no LongarmUpdate service' }
  & (Join-Path $installDir 'longarm.exe') version 2>&1 | Write-Host
  Write-Host "installed: Longarm $version, files as built, firewall rules once each"
}

function Invoke-Upgrade([string] $what, [string] $msi, [string] $arguments, [string] $version, [string] $hashes) {
  $before = @(Get-LongarmProcs)
  $svcBefore = Get-ServicePids
  $kinds = @($before | Where-Object { $restartKinds -contains $_.Kind } | ForEach-Object Kind | Sort-Object -Unique)
  $job = @($before | Where-Object Kind -EQ 'job' | ForEach-Object ProcessId)
  $job += @(Get-CimInstance Win32_Process | Where-Object { $job -contains $_.ParentProcessId } | ForEach-Object ProcessId)
  Write-Host "running before the upgrade: kinds $($kinds -join ', '); services $(($svcBefore.Keys | Sort-Object) -join ', '); a job's processes $($job -join ', ')"
  if ($job.Count -lt 2) { Fail "the job (longarmd conhost and its command) does not run: $($job -join ', ')" }
  if (-not $kinds.Count) { Fail 'nothing runs before the upgrade: the test would prove nothing' }
  $text = Invoke-Msi $what "/i `"$msi`" $arguments"
  if ($text -notmatch 'Disallowing uninstallation of component') { Write-Host '::warning::no component was shared with the old product' }
  Assert-Installed $version $hashes
  # stopped: none of the old processes of those kinds is left
  $old = @($before | Where-Object { $restartKinds -contains $_.Kind } | ForEach-Object ProcessId)
  $left = @(Get-LongarmProcs | Where-Object { $old -contains $_.ProcessId })
  if ($left.Count) { Fail "still running after the upgrade (not stopped): $($left.CommandLine -join '; ')" }
  # started again: each kind, as a new process
  foreach ($k in $kinds) {
    if (-not (Wait-Until { @(Get-LongarmProcs | Where-Object { $_.Kind -eq $k -and $old -notcontains $_.ProcessId }).Count } 90 "a new $k")) {
      Fail "$k was running before the upgrade and was not started again"
    }
  }
  foreach ($s in $svcBefore.Keys) {
    if (-not (Wait-Until { $n = (Get-ServicePids)[$s]; $n -and $n -ne $svcBefore[$s] } 90 "service $s")) {
      Fail "service $s ran before the upgrade and does not run again as a new process"
    }
  }
  # left running: the job (its console host and command), longarmd.exe moved aside
  foreach ($j in $job) {
    if (-not (Get-Process -Id $j -ErrorAction SilentlyContinue)) { Fail "the job's process $j was ended by the upgrade" }
  }
  $aside = @(Get-ChildItem (Join-Path $installDir 'old') -Filter 'longarmd-*.exe' -ErrorAction SilentlyContinue)
  if (-not $aside.Count) { Fail "the job's longarmd.exe was not moved aside to $installDir\old" }
  Write-Host "the job ($($job -join ', ')) still runs; $installDir\old: $($aside.Name -join ', ')"
  $rec = Join-Path $dataDir 'msi-upgrade.json'
  if (Test-Path $rec) { Write-Host "--- $rec"; Get-Content $rec | Write-Host }
  Write-Host "after the upgrade:"
  Get-LongarmProcs | Format-Table -AutoSize ProcessId, Kind, SessionId, CommandLine | Out-String -Width 300 | Write-Host
  Write-Host "$what`: OK - stopped and started again: $($kinds + @($svcBefore.Keys) -join ', ')"
}

function Invoke-Uninstall {
  # the job is ours (a real one keeps its moved-aside binary until it ends): end it and its command by pid
  foreach ($j in @(Get-LongarmProcs | Where-Object Kind -EQ 'job')) {
    Get-CimInstance Win32_Process | Where-Object ParentProcessId -EQ $j.ProcessId | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    Stop-Process -Id $j.ProcessId -Force -ErrorAction SilentlyContinue
  }
  $code = (Get-Products)[0].PSChildName
  # leaving the hub fails with the dummy hub (the uninstall goes on: Return="ignore")
  $null = Invoke-Msi 'uninstall' "/x $code" -allow @('returned actual error code')
  if ((Get-Products).Count) { Fail 'Longarm is still registered after the uninstall' }
  foreach ($f in 'longarm.exe', 'longarmd.exe', 'longarm-tray.exe', 'longarm-update.exe') {
    if (Test-Path (Join-Path $installDir $f)) { Fail "$f is left after the uninstall" }
  }
  if (Get-Service LongarmUpdate -ErrorAction SilentlyContinue) { Fail 'the LongarmUpdate service is left' }
  if (Get-ScheduledTask -TaskName "LongarmWorker-$sid" -ErrorAction SilentlyContinue) { Fail 'the worker task is left' }
  $left = @(Get-LongarmProcs)
  if ($left.Count) { Fail "processes left after the uninstall: $($left.CommandLine -join '; ')" }
  if (@(Get-NetFirewallRule -DisplayName 'Longarm longarm (UDP)' -ErrorAction SilentlyContinue).Count) { Fail 'a firewall rule is left' }
  Write-Host 'uninstall: OK'
}

# --- scenarios -------------------------------------------------------------------------------------------

$default = Join-Path $MsiDir 'default.msi'
$next = Join-Path $MsiDir 'next.msi'
if ((Get-Products).Count) { Fail 'Longarm is already installed on this runner' }

switch ($Scenario) {
  'fresh' {
    $null = Invoke-Msi 'install' "/i `"$default`" ROLES=both"
    Assert-Installed '0.250.0' (Join-Path $MsiDir 'hashes-default.txt')
    Start-Running $true $true
    # a repair (as Windows' "Repair", which the MSI hides: msiexec /fomus)
    $code = (Get-Products)[0].PSChildName
    $null = Invoke-Msi 'repair' "/fomus $code"
    Assert-Installed '0.250.0' (Join-Path $MsiDir 'hashes-default.txt')
    Start-Running $true $true
    # an upgrade can't switch the worker to a service: refused before anything changes or stops
    $running = @(Get-LongarmProcs | Where-Object { $restartKinds -contains $_.Kind } | ForEach-Object ProcessId)
    $text = Invoke-Msi 'refused-switch' "/i `"$next`" RUN=service SERVICE_ACCOUNT=LocalSystem" -ok @(1603) -allow @('1603', 'Return value 3', 'Installation failed', 'MainEngineThread is returning 1603')
    if ($text -notmatch 'switch the worker between a Windows service and the user-session worker') { Fail 'the refused upgrade did not say why' }
    $gone = @($running | Where-Object { -not (Get-Process -Id $_ -ErrorAction SilentlyContinue) })
    if ($gone.Count) { Fail "the refused upgrade ended processes $($gone -join ', ')" }
    Assert-Installed '0.250.0' (Join-Path $MsiDir 'hashes-default.txt')
    Start-Running $true $true
    Invoke-Upgrade 'upgrade-to-next' $next '' '0.250.1' (Join-Path $MsiDir 'hashes-next.txt')
    Invoke-Uninstall
  }
  'upgrade' {
    if (-not $OldMsi -or -not (Test-Path $OldMsi)) { Fail "no -OldMsi ($OldMsi)" }
    # (an older version's Return="ignore" steps may fail here, e.g. starting the tray: not what is tested)
    $null = Invoke-Msi 'install-old' "/i `"$OldMsi`" $OldArgs" -allow @('returned actual error code')
    $old = Get-Products
    Write-Host "installed: Longarm $($old.DisplayVersion) ($OldArgs)"
    $session = $OldArgs -notmatch 'RUN=service'
    Start-Running $true $session
    Invoke-Upgrade "upgrade-from-$($old.DisplayVersion)" $default '' '0.250.0' (Join-Path $MsiDir 'hashes-default.txt')
    if ($OldArgs -match 'ADMIN_WORKER=1' -and -not (Get-Service longarmd-admin -ErrorAction SilentlyContinue)) { Fail 'the admin worker service is gone after the upgrade' }
    Invoke-Uninstall
  }
}
Write-Host "scenario $Scenario passed"
