<#
.SYNOPSIS
  Checks where an MSI's InstallExecuteSequence runs RemoveExistingProducts (reads the built tables; no install).
.DESCRIPTION
  Windows Installer allows RemoveExistingProducts only:
    - between InstallValidate and InstallInitialize (outside the transaction),
    - after InstallInitialize but before anything that writes to the install script: no deferred,
      rollback or commit custom action and no standard action between them (error 2613 "RemoveExistingProducts
      action sequenced incorrectly" otherwise - the Longarm 0.1.8 to 0.1.13 MSIs: a deferred stop step before it),
    - after InstallExecute / InstallExecuteAgain (the script so far has run) and before InstallFinalize,
    - after InstallFinalize.
  Exits with an error naming the offending actions. -ExpectFailure inverts the result (CI shows that a shipped
  MSI with the bug is caught).
.PARAMETER Msi
  The MSI to check.
#>
param([Parameter(Mandatory)] [string] $Msi, [switch] $ExpectFailure)
$ErrorActionPreference = 'Stop'

function Rows([string] $sql, [int] $fields) {
  $inst = New-Object -ComObject WindowsInstaller.Installer
  $db = $inst.GetType().InvokeMember('OpenDatabase', 'InvokeMethod', $null, $inst, @((Resolve-Path $Msi).Path, 0))
  $view = $db.GetType().InvokeMember('OpenView', 'InvokeMethod', $null, $db, @($sql))
  $view.GetType().InvokeMember('Execute', 'InvokeMethod', $null, $view, $null) | Out-Null
  $out = @()
  while ($true) {
    $rec = $view.GetType().InvokeMember('Fetch', 'InvokeMethod', $null, $view, $null)
    if (-not $rec) { break }
    $row = @()
    for ($i = 1; $i -le $fields; $i++) { $row += $rec.GetType().InvokeMember('StringData', 'GetProperty', $null, $rec, $i) }
    $out += , $row
  }
  $view.GetType().InvokeMember('Close', 'InvokeMethod', $null, $view, $null) | Out-Null
  return , $out
}

function Test-Sequence {
  $seq = @{}
  foreach ($r in (Rows 'SELECT `Action`, `Sequence` FROM `InstallExecuteSequence`' 2)) {
    if ($r[1] -ne '') { $seq[$r[0]] = [int]$r[1] }
  }
  $types = @{}
  foreach ($r in (Rows 'SELECT `Action`, `Type` FROM `CustomAction`' 2)) { $types[$r[0]] = [int]$r[1] }
  if (-not $seq.ContainsKey('RemoveExistingProducts')) { return 'no RemoveExistingProducts' }
  $rep = $seq['RemoveExistingProducts']
  $init, $validate, $final = $seq['InstallInitialize'], $seq['InstallValidate'], $seq['InstallFinalize']
  $exec = @('InstallExecute', 'InstallExecuteAgain' | Where-Object { $seq.ContainsKey($_) } | ForEach-Object { $seq[$_] })
  Write-Host "RemoveExistingProducts $rep (InstallValidate $validate, InstallInitialize $init, InstallExecute(Again) $($exec -join '/'), InstallFinalize $final)"
  if ($rep -lt $init) {
    if ($rep -gt $validate) { return $null }
    return "RemoveExistingProducts ($rep) before InstallValidate ($validate)"
  }
  if ($rep -gt $final) { return $null }
  if (@($exec | Where-Object { $_ -lt $rep }).Count) { return $null } # the script so far has run
  # inside the transaction, before InstallExecute: nothing may write to the script before it
  $bad = @()
  foreach ($a in $seq.Keys) {
    $s = $seq[$a]
    if ($s -le $init -or $s -ge $rep) { continue }
    if ($types.ContainsKey($a)) {
      if ($types[$a] -band 0x400) { $bad += "$a ($s, deferred/rollback/commit custom action, type $($types[$a]))" }
    } else {
      $bad += "$a ($s, standard action)"
    }
  }
  if ($bad.Count) {
    return "RemoveExistingProducts ($rep) comes after actions that write to the install script (error 2613): " + ($bad -join ', ')
  }
  return $null
}

$problem = Test-Sequence
if ($ExpectFailure) {
  if (-not $problem) { throw "$Msi passes the RemoveExistingProducts check, but it was expected to fail" }
  Write-Host "${Msi}: caught as expected: $problem"
  exit 0
}
if ($problem) { throw "${Msi}: $problem" }
Write-Host "${Msi}: RemoveExistingProducts is sequenced correctly"
