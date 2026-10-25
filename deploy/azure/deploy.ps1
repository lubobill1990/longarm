<#
.SYNOPSIS
  longarm-hub on Azure Container Instances in one command (deploy/azure/README.md).

.DESCRIPTION
  The PowerShell twin of deploy.sh: creates the resource group, deploys
  azuredeploy.json (storage, Litestream replica container, state share,
  the container group), waits for the hub and prints what to run on the
  admin's laptop and on workers. Re-running is an upgrade (data kept).
  Needs the Azure CLI (az login); works in Azure Cloud Shell (pwsh) and in
  Windows PowerShell 5.1.

.EXAMPLE
  ./deploy.ps1 -Name myhub -Location eastasia
.EXAMPLE
  ./deploy.ps1 -Name myhub -Image ghcr.io/<owner>/longarm-hub:1.2.3
.EXAMPLE
  ./deploy.ps1 -Name myhub -Teardown
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-z][a-z0-9-]{1,38}[a-z0-9]$')][string]$Name,
    [string]$Location = '',
    [ValidateSet('small', 'medium')][string]$Size = 'small',
    [switch]$NoPanel,
    [string]$ResourceGroup = '',
    [string]$Image = '',
    # private image: the password comes from $env:LONGARM_REGISTRY_PASSWORD
    [string]$RegistryUser = '',
    [string]$TailscaleSecretFile = '',
    [string]$AcmeEmail = '',
    [switch]$LogAnalytics,
    [string]$Subscription = '',
    # delete the container group first (to change -Size or -NoPanel)
    [switch]$Recreate,
    # print a new one-time admin join code
    [switch]$AdminCode,
    [switch]$Teardown,
    [switch]$Yes,
    [int]$TimeoutSeconds = 900
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0

# the Azure CLI; tests put a fake here (e2e/azure_deploy_test.go)
$script:AzCli = 'az'
if ($env:LONGARM_DEPLOY_AZ) { $script:AzCli = $env:LONGARM_DEPLOY_AZ }

function Invoke-Az {
    # az with its exit status checked; returns stdout lines
    $out = & $script:AzCli @args
    if ($LASTEXITCODE -ne 0) { throw "az $($args[0]) $($args[1]) failed (exit $LASTEXITCODE)" }
    $out
}

function Test-Az {
    # az for a yes/no question: $true on exit 0, output discarded
    Invoke-AzQuiet @args | Out-Null
    $LASTEXITCODE -eq 0
}

function Invoke-AzQuiet {
    # az whose failure is an answer, not an error: stdout only, $LASTEXITCODE
    # set (Windows PowerShell 5.1 turns redirected native stderr into errors,
    # fatal under ErrorActionPreference Stop)
    $ErrorActionPreference = 'Continue'
    & $script:AzCli @args 2>$null
}

function Test-TcpPort([string]$HostName, [int]$Port) {
    $c = New-Object System.Net.Sockets.TcpClient
    try {
        $task = $c.ConnectAsync($HostName, $Port)
        return ($task.Wait(5000) -and $c.Connected)
    } catch {
        return $false
    } finally {
        $c.Dispose()
    }
}

if (-not $ResourceGroup) { $ResourceGroup = "$Name-rg" }
$rg = $ResourceGroup
$template = Join-Path $PSScriptRoot 'azuredeploy.json'

if (-not (Get-Command $script:AzCli -ErrorAction SilentlyContinue)) {
    throw 'the Azure CLI (az) is missing: https://aka.ms/azure-cli (or use Azure Cloud Shell)'
}
if ($Subscription) { Invoke-Az account set --subscription $Subscription | Out-Null }
if (-not (Test-Az account show -o none)) { throw 'not logged in: run az login' }

function Test-ResourceGroup { (Invoke-Az group exists -n $rg -o tsv) -eq 'true' }

if ($Teardown) {
    if (-not (Test-ResourceGroup)) { throw "resource group $rg does not exist" }
    $tags = (Invoke-Az group show -n $rg --query tags -o json) -join "`n" | ConvertFrom-Json
    $tag = ''
    if ($tags -and $tags.PSObject.Properties['longarm-hub']) { $tag = $tags.'longarm-hub' }
    if ($tag -ne $Name) { throw "resource group $rg was not created by deploy.ps1 for $Name (tag longarm-hub=$tag): delete it yourself" }
    if (-not $Yes) {
        Write-Host "This deletes resource group ${rg}: the hub, its database replica, the CA and all state."
        $answer = Read-Host "Type the hub name ($Name) to continue"
        if ($answer -ne $Name) { throw 'aborted' }
    }
    Invoke-Az group delete -n $rg --yes | Out-Null
    Write-Host "deleted $rg"
    return
}

if ($AdminCode) {
    Write-Host 'New one-time admin join code (valid 24 h):'
    Invoke-Az container exec -g $rg -n $Name --container-name hub --exec-command '/usr/local/bin/longarm-hub join-code --role admin --ttl 24h'
    return
}

if (-not (Test-Path $template)) { throw "$template is missing (run from a checkout of the repository)" }

if (Test-ResourceGroup) {
    if (-not $Location) { $Location = Invoke-Az group show -n $rg --query location -o tsv }
} else {
    if (-not $Location) { throw '-Location is required for a new deployment (az account list-locations -o table)' }
    Write-Host "creating resource group $rg in $Location"
    Invoke-Az group create -n $rg -l $Location --tags "longarm-hub=$Name" -o none | Out-Null
}

$hubImageQuery = "containers[?name=='hub'].image"
$prevImage = ''
if (Test-Az container show -g $rg -n $Name -o none) {
    $prevImage = Invoke-Az container show -g $rg -n $Name --query $hubImageQuery -o tsv
}
if ($Recreate -and $prevImage) {
    Write-Host "deleting container group $Name (storage and data stay)"
    Invoke-Az container delete -g $rg -n $Name --yes -o none | Out-Null
    $prevImage = ''
}

# parameters in a private file: no secrets on the command line
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("longarm-deploy-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $p = [ordered]@{
        name               = @{ value = $Name }
        location           = @{ value = $Location }
        size               = @{ value = $Size }
        enablePanel        = @{ value = (-not $NoPanel) }
        enableLogAnalytics = @{ value = [bool]$LogAnalytics }
    }
    if ($Image) { $p.image = @{ value = $Image } }
    if ($AcmeEmail) { $p.acmeEmail = @{ value = $AcmeEmail } }
    if ($RegistryUser) {
        if (-not $env:LONGARM_REGISTRY_PASSWORD) { throw '-RegistryUser needs $env:LONGARM_REGISTRY_PASSWORD' }
        $p.registryUsername = @{ value = $RegistryUser }
        $p.registryPassword = @{ value = $env:LONGARM_REGISTRY_PASSWORD }
    }
    if ($TailscaleSecretFile) {
        $p.tailscaleOAuthSecret = @{ value = (Get-Content -Raw $TailscaleSecretFile).Trim() }
    }
    $paramsFile = Join-Path $tmp 'params.json'
    $doc = @{ contentVersion = '1.0.0.0'; parameters = $p }
    [System.IO.File]::WriteAllText($paramsFile, ($doc | ConvertTo-Json -Depth 5))

    Write-Host "deploying $Name to $rg ($Location, $Size, panel $(-not $NoPanel)); this takes a few minutes"
    $query = '[properties.outputs.hubAddress.value, properties.outputs.fqdn.value, properties.outputs.storageAccount.value, properties.outputs.panelUrl.value]'
    # one tab-separated line; panelUrl last (empty without the panel)
    $outLine = (Invoke-Az deployment group create -g $rg -n longarm-hub --template-file $template --parameters "@$paramsFile" --query $query -o tsv) -join ''
    $outs = @($outLine.Trim("`r", "`n") -split "`t") + @('', '', '', '')
    $hubAddr, $fqdn, $account, $panelUrl = $outs[0], $outs[1], $outs[2], $outs[3]
    if (-not $hubAddr -or -not $fqdn -or -not $account) { throw "deployment returned no outputs: $outLine" }

    $newImage = Invoke-Az container show -g $rg -n $Name --query $hubImageQuery -o tsv
    if ($prevImage -and $prevImage -eq $newImage -and $newImage -match ':(latest|edge)$') {
        Write-Host "pulling $newImage again (az container restart)"
        Invoke-Az container restart -g $rg -n $Name -o none | Out-Null
    }

    Write-Host "waiting for the hub at $hubAddr"
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    $logsHint = "az container logs -g $rg -n $Name --container-name hub"
    while ($true) {
        $state = Invoke-AzQuiet container show -g $rg -n $Name --query "containers[?name=='hub'].instanceView.currentState.state" -o tsv
        if ($state -eq 'Running') { break }
        if ((Get-Date) -gt $deadline) { throw "the hub container is not running ($state): $logsHint" }
        Start-Sleep -Seconds 10
    }
    $port = [int]($hubAddr.Substring($hubAddr.LastIndexOf(':') + 1))
    while (-not (Test-TcpPort $fqdn $port)) {
        if ((Get-Date) -gt $deadline) { throw "$hubAddr does not answer: $logsHint" }
        Start-Sleep -Seconds 5
    }

    # bootstrap.env from the state share (else the container log)
    $key = Invoke-Az storage account keys list -g $rg -n $account --query '[0].value' -o tsv
    $bootFile = Join-Path $tmp 'bootstrap.env'
    $boot = @()
    while ($boot.Count -eq 0) {
        Invoke-AzQuiet storage file download --account-name $account --account-key $key --share-name state --path bootstrap.env --dest $bootFile --no-progress -o none | Out-Null
        if ($LASTEXITCODE -eq 0 -and (Test-Path $bootFile)) {
            $boot = @(Get-Content $bootFile)
        } else {
            $line = @(Invoke-AzQuiet container logs -g $rg -n $Name --container-name hub | Where-Object { $_ -like 'LONGARM_BOOTSTRAP *' } | Select-Object -Last 1)
            if ($line.Count -gt 0) { $boot = @($line[0].Substring('LONGARM_BOOTSTRAP '.Length) -split ' ') }
        }
        if ($boot.Count -eq 0) {
            if ((Get-Date) -gt $deadline) { throw "no bootstrap.env on the share yet: $logsHint" }
            Start-Sleep -Seconds 5
        }
    }
    $kv = @{}
    foreach ($l in $boot) {
        $i = $l.IndexOf('=')
        if ($i -gt 0 -and -not $l.StartsWith('#')) { $kv[$l.Substring(0, $i).Trim()] = $l.Substring($i + 1).Trim() }
    }
    $fp = $kv['ca_fingerprint']
    $code = $kv['admin_join_code']
    $expires = $kv['admin_join_code_expires']

    if ($panelUrl) {
        $ok = $false
        for ($i = 0; $i -lt 18 -and -not $ok; $i++) {
            try {
                Invoke-WebRequest -UseBasicParsing -TimeoutSec 10 -Uri "$panelUrl/healthz" | Out-Null
                $ok = $true
            } catch {
                Start-Sleep -Seconds 10
            }
        }
        if (-not $ok) { Write-Warning "$panelUrl/healthz does not answer yet (Let's Encrypt can take a few minutes): az container logs -g $rg -n $Name --container-name caddy" }
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# Rough monthly cost (Linux ACI pay-as-you-go, about 0.0405 USD per vCPU-hour
# and 0.0045 USD per GB-hour in US regions; other regions differ by up to ~20 %).
$cpu = 1.0; $mem = 1.5
if ($Size -eq 'medium') { $cpu = 2.0; $mem = 3.0 }
if (-not $NoPanel) { $cpu += 0.25; $mem += 0.25 }
$cost = [math]::Round(730 * ($cpu * 0.0405 + $mem * 0.0045) + 1)

$panelText = if ($panelUrl) { $panelUrl } else { 'off (-NoPanel)' }
Write-Host @"

longarm-hub is up.
  hub address     $hubAddr
  CA fingerprint  $fp
  panel           $panelText
  resource group  $rg (storage account $account)
  cost            about $cost USD/month (compute; plus egress over 100 GB/month, relay traffic counts)

"@
if ($code) {
    Write-Host @"
1. Your laptop (admin): install longarm (https://github.com/lubobill1990/longarm/releases), then
     longarm join $hubAddr $code --ca-fingerprint $fp
   The code is one-time and valid until $expires (a new one: ./deploy.ps1 -Name $Name -AdminCode).
"@
} else {
    Write-Host "1. An admin has joined already. Another admin: ./deploy.ps1 -Name $Name -AdminCode"
}
Write-Host @"

2. Each worker: a code from the laptop, then on the worker
     longarm admin join-code --role worker
     longarmd join $hubAddr <worker-code> --ca-fingerprint $fp --name worker-1
     longarm admin approve worker-1                 # on the laptop
     longarmd service install --user .\<you>; longarmd service start   # elevated (Linux/macOS: sudo ... --user `$USER)
"@
if ($panelUrl) {
    Write-Host @"
   Or, once a release is on the hub (longarm admin release publish, docs/usage.md):
     curl -fsSL $panelUrl/install.sh | sh -s -- --join <worker-code>                      # Linux/macOS
     `$env:LONGARM_JOIN='<worker-code>'; irm $panelUrl/install.ps1 | iex                   # Windows
   Panel login: az container exec -g $rg -n $Name --container-name hub --exec-command "/usr/local/bin/longarm-hub user add admin"
"@
}
Write-Host @"

3. Your agents:
     claude mcp add longarm -- longarm mcp

Upgrade: ./deploy.ps1 -Name $Name -Image <image:tag>    Remove: ./deploy.ps1 -Name $Name -Teardown
"@
