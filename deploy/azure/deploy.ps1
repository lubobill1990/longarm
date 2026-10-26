<#
.SYNOPSIS
  longarm-hub on Azure Container Instances in one command (deploy/azure/README.md).

.DESCRIPTION
  The PowerShell twin of deploy.sh: creates the resource group, deploys
  azuredeploy.json (storage, Litestream replica container, state share,
  the container group), waits for the hub and prints what to run on the
  admin's laptop and on workers. Re-running is an upgrade: the running
  hub's settings (image, size, panel, ...) are kept unless a parameter
  changes them, the data stays, the hub restarts.
  Needs the Azure CLI (az login); works in Azure Cloud Shell (pwsh) and in
  Windows PowerShell 5.1.

.EXAMPLE
  ./deploy.ps1 -Name myhub -Location eastasia
.EXAMPLE
  ./deploy.ps1 -Name myhub -Image ghcr.io/<owner>/longarm-hub:1.2.3
.EXAMPLE
  ./deploy.ps1 -Name myhub -Teardown
.EXAMPLE
  ./deploy.ps1 -Name myhub -Location eastasia -BuildInAcr   # image built from this checkout in your ACR
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-z][a-z0-9-]{1,38}[a-z0-9]$')][string]$Name,
    [string]$Location = '',
    # small = 1 vCPU / 1.5 GB (first run default), medium = 2 vCPU / 3 GB
    [ValidateSet('', 'small', 'medium')][string]$Size = '',
    # without / with the HTTPS panel (first run default: with)
    [switch]$NoPanel,
    [switch]$Panel,
    [string]$ResourceGroup = '',
    [string]$Image = '',
    # private image: the password comes from $env:LONGARM_REGISTRY_PASSWORD
    [string]$RegistryUser = '',
    # build the image from this checkout with az acr build (a registry in the resource group) and deploy it
    [switch]$BuildInAcr,
    # external STUN servers the hub advertises (host:port,...; 'none' = only the hub's own)
    [string]$StunServers = '',
    [string]$TailscaleSecretFile = '',
    [string]$AcmeEmail = '',
    [switch]$LogAnalytics,
    [string]$Subscription = '',
    # delete the container group first (to change -Size or the panel)
    [switch]$Recreate,
    # print a new one-time admin join code
    [switch]$AdminCode,
    [switch]$Teardown,
    [switch]$Yes,
    [int]$TimeoutSeconds = 1200,
    # give up on a container group stuck in Creating without events
    [int]$StuckTimeoutSeconds = 600
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0

# the Azure CLI; tests put a fake here (e2e/azure_deploy_test.go)
$script:AzCli = 'az'
if ($env:LONGARM_DEPLOY_AZ) { $script:AzCli = $env:LONGARM_DEPLOY_AZ }
# seconds between status polls (tests make it short)
$script:Poll = 10
if ($env:LONGARM_DEPLOY_POLL_SECONDS) { $script:Poll = [int]$env:LONGARM_DEPLOY_POLL_SECONDS }

function Invoke-Az {
    # az with its exit status checked; returns stdout lines
    $out = & $script:AzCli @args
    if ($LASTEXITCODE -ne 0) { throw "az $($args[0]) $($args[1]) failed (exit $LASTEXITCODE)" }
    $out
}

function Invoke-AzQuiet {
    # az whose failure is an answer, not an error: stdout only, $LASTEXITCODE
    # set (Windows PowerShell 5.1 turns redirected native stderr into errors,
    # fatal under ErrorActionPreference Stop)
    $ErrorActionPreference = 'Continue'
    & $script:AzCli @args 2>$null
}

function Test-Az {
    # az for a yes/no question: $true on exit 0, output discarded
    Invoke-AzQuiet @args | Out-Null
    $LASTEXITCODE -eq 0
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

function Get-Bootstrap([string]$Account, [string]$Key, [string]$Dest) {
    # bootstrap.env from the state share as a hashtable ($null if missing);
    # the key goes through the environment, not the command line
    Remove-Item -Force $Dest -ErrorAction SilentlyContinue
    # az prefers a connection string / SAS from the caller's environment over the key: hide them for
    # this call (a global AZURE_STORAGE_CONNECTION_STRING for another account gave ShareNotFound)
    $hidden = @{}
    foreach ($n in 'AZURE_STORAGE_CONNECTION_STRING', 'AZURE_STORAGE_SAS_TOKEN', 'AZURE_STORAGE_ACCOUNT', 'AZURE_STORAGE_AUTH_MODE') {
        $v = [Environment]::GetEnvironmentVariable($n, 'Process')
        if ($null -ne $v) { $hidden[$n] = $v; [Environment]::SetEnvironmentVariable($n, $null, 'Process') }
    }
    $env:AZURE_STORAGE_KEY = $Key
    try {
        Invoke-AzQuiet storage file download --account-name $Account --share-name state --path bootstrap.env --dest $Dest --no-progress -o none | Out-Null
    } finally {
        Remove-Item Env:\AZURE_STORAGE_KEY -ErrorAction SilentlyContinue
        foreach ($n in $hidden.Keys) { [Environment]::SetEnvironmentVariable($n, $hidden[$n], 'Process') }
    }
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path $Dest)) { return $null }
    # the file holds the admin join code: read it and remove it at once
    $lines = Get-Content $Dest
    Remove-Item $Dest -Force -ErrorAction SilentlyContinue
    $kv = @{}
    foreach ($l in $lines) {
        $i = $l.IndexOf('=')
        if ($i -gt 0 -and -not $l.StartsWith('#')) { $kv[$l.Substring(0, $i).Trim()] = $l.Substring($i + 1).Trim() }
    }
    if ($kv.Count -eq 0) { return $null }
    $kv
}

function Get-Value($Map, [string]$Key) {
    if ($Map -and $Map.ContainsKey($Key)) { return $Map[$Key] }
    ''
}

function Register-Provider([string[]]$Namespaces) {
    # a new subscription has e.g. Microsoft.ContainerInstance NotRegistered:
    # the deployment would fail with MissingSubscriptionRegistration
    foreach ($ns in $Namespaces) {
        $state = (Invoke-AzQuiet provider show -n $ns --query registrationState -o tsv) -join ''
        if ($state -eq 'Registered') { continue }
        Write-Host "registering the resource provider $ns ($state; once per subscription, a minute or two)"
        Invoke-AzQuiet provider register -n $ns -o none | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "az provider register -n $ns failed (needs Contributor on the subscription; or ask its owner to register it)" }
        $until = (Get-Date).AddMinutes(10)
        while ($state -ne 'Registered') {
            if ((Get-Date) -gt $until) { throw "$ns is still $state after 10 min: az provider show -n $ns" }
            Start-Sleep -Seconds $script:Poll
            $state = (Invoke-AzQuiet provider show -n $ns --query registrationState -o tsv) -join ''
        }
    }
}

function Get-RegistryHost([string]$Ref) {
    # the registry of an image reference ('' = Docker Hub)
    if ($Ref -notlike '*/*') { return '' }
    $h = $Ref.Substring(0, $Ref.IndexOf('/'))
    if ($h -like '*.*' -or $h -like '*:*' -or $h -eq 'localhost') { return $h }
    ''
}

$script:StuckSince = $null
$script:StuckTimeout = $StuckTimeoutSeconds
function Test-Stuck {
    # throws when the container group has sat in Creating with waiting
    # containers and no events at all for -StuckTimeoutSeconds (seen once on
    # a real run: 30+ min, while a recreate came up in 3.5 min)
    $q = "[provisioningState, containers[?name=='hub'].instanceView.currentState.state | [0], length(containers[].instanceView.events[] || ``[]``), length(instanceView.events || ``[]``)]"
    $line = (Invoke-AzQuiet container show -g $rg -n $Name -o tsv --query $q) -join ''
    if ($LASTEXITCODE -ne 0 -or -not $line) { return }
    $f = @($line.Trim("`r", "`n") -split "`t") + @('', '', '', '')
    $events = 0
    foreach ($x in $f[2], $f[3]) { if ($x -match '^\d+$') { $events += [int]$x } }
    if (($f[0] -eq 'Creating' -or $f[0] -eq 'Pending') -and ($f[1] -eq '' -or $f[1] -eq 'Waiting') -and $events -eq 0) {
        if (-not $script:StuckSince) { $script:StuckSince = Get-Date }
        $secs = [int]((Get-Date) - $script:StuckSince).TotalSeconds
        if ($secs -ge $script:StuckTimeout) {
            throw @"
the container group $Name has been $($f[0]) for $secs s with its containers waiting and no events at all.
That is an Azure Container Instances hiccup, not the template (seen once: the same deployment came up in 3.5 min after a recreate).
  retry:             ./deploy.ps1 -Name $Name -Recreate            (storage and data are kept)
  without the panel: ./deploy.ps1 -Name $Name -NoPanel -Recreate
  look:              az container show -g $rg -n $Name -o table
"@
        }
    } else {
        $script:StuckSince = $null
    }
}

if ($NoPanel -and $Panel) { throw '-NoPanel and -Panel exclude each other' }
if ($TailscaleSecretFile -and -not (Test-Path $TailscaleSecretFile)) { throw "-TailscaleSecretFile: can't read $TailscaleSecretFile" }
if (-not $ResourceGroup) { $ResourceGroup = "$Name-rg" }
if ($BuildInAcr -and $Image) { throw '-BuildInAcr builds the image: drop -Image' }
# none alone, or host:port / [v6]:port entries (the hub refuses a missing port: the container would crash-loop)
if ($StunServers -and $StunServers -ne 'none') {
    foreach ($s in $StunServers.Split(',')) {
        if ($s -notmatch '^([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\]):[0-9]{1,5}$') { throw "-StunServers: '$s' is not host:port or [v6]:port (none goes alone)" }
    }
}
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
    # az container exec runs one program without arguments: an alias of
    # longarm-hub join-code --role admin --ttl 24h (cmd/longarm-hub/container.go)
    Write-Host 'A new one-time admin join code (valid 24 h):'
    Invoke-Az container exec -g $rg -n $Name --container-name hub --exec-command /usr/local/bin/longarm-hub-admin-code
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
Register-Provider 'Microsoft.ContainerInstance', 'Microsoft.Storage'

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("longarm-deploy-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
if ($PSVersionTable.PSEdition -eq 'Core' -and -not $IsWindows) { & chmod 700 $tmp }
try {
    $bootFile = Join-Path $tmp 'bootstrap.env'
    $panelOn = $null
    if ($NoPanel) { $panelOn = $false }
    if ($Panel) { $panelOn = $true }
    $logsOn = [bool]$LogAnalytics

    # A re-run keeps the running hub's settings. Secrets can't be read back
    # from ACI: a hub that has them needs them again.
    $curQuery = "[containers[0].image, containers[0].resources.requests.cpu, containers[1].name, imageRegistryCredentials[0].server, join(',', containers[0].environmentVariables[].name), diagnostics.logAnalytics.workspaceId, containers[0].environmentVariables[?name=='LONGARM_HUB_RELAY_STUN_ADVERTISE'].value | [0]]"
    $curLine = (Invoke-AzQuiet container show -g $rg -n $Name -o tsv --query $curQuery) -join ''
    $curImage = ''
    $curRegistry = ''
    $prevBoot = ''
    if ($LASTEXITCODE -eq 0 -and $curLine) {
        $f = @($curLine.Trim("`r", "`n") -split "`t") + @('', '', '', '', '', '', '')
        $curImage = $f[0]
        $curSize = 'small'
        if ([double]$f[1] -ge 2) { $curSize = 'medium' }
        $curPanel = ($f[2] -eq 'caddy')
        if (-not $Recreate) {
            if ($Size -and $Size -ne $curSize) { throw "the hub is $curSize; ACI can't resize a running container group: add -Recreate (data is kept)" }
            if ($null -ne $panelOn -and $panelOn -ne $curPanel) { throw "ACI can't change the ports of a running group (panel on/off): add -Recreate (data is kept)" }
        }
        if (-not $Image) { $Image = $curImage }
        if (-not $Size) { $Size = $curSize }
        if ($null -eq $panelOn) { $panelOn = $curPanel }
        if ($f[5]) { $logsOn = $true }
        # the template sets self[,<stunServers>]; absent = a hub from before (template default)
        if (-not $StunServers) {
            if ($f[6] -eq 'self') { $StunServers = 'none' }
            elseif ($f[6] -like 'self,*') { $StunServers = $f[6].Substring(5) }
        }
        $curRegistry = $f[3]
        if ((",$($f[4]),") -like '*,LONGARM_HUB_TAILSCALE_OAUTH_SECRET,*' -and -not $TailscaleSecretFile) { throw "the hub has a Tailscale OAuth secret, which can't be read back: pass -TailscaleSecretFile again" }
        # this start's bootstrap.env must differ from the running one's (boot_id)
        $acct = @(Invoke-Az storage account list -g $rg --query "[?starts_with(name, 'lahub')].name" -o tsv) | Select-Object -First 1
        if ($acct) {
            $k = Invoke-Az storage account keys list -g $rg -n $acct --query '[0].value' -o tsv
            $prevBoot = Get-Value (Get-Bootstrap $acct $k $bootFile) 'boot_id'
        }
    }
    if (-not $Size) { $Size = 'small' }
    if ($null -eq $panelOn) { $panelOn = $true }
    if ($logsOn) { Register-Provider 'Microsoft.OperationalInsights' }

    $acr = ''
    $registryPassword = $env:LONGARM_REGISTRY_PASSWORD
    if ($BuildInAcr) {
        # the image from this checkout, built by ACR Tasks (no docker here); a
        # registry in the hub's resource group, created on the first use
        Register-Provider 'Microsoft.ContainerRegistry'
        $root = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
        if (-not (Test-Path (Join-Path $root 'deploy/hub/Dockerfile')) -or -not (Test-Path (Join-Path $root 'go.mod'))) { throw '-BuildInAcr: run from a checkout of the repository' }
        # filtered here: an embedded " in --query breaks with pwsh 7.3+ native argument passing
        $acr = ''
        $regs = ((Invoke-Az acr list -g $rg -o json) -join "`n") | ConvertFrom-Json
        foreach ($r in @($regs)) {
            if ($null -ne $r -and $r.PSObject.Properties['tags'] -and $r.tags -and $r.tags.PSObject.Properties['longarm-hub'] -and $r.tags.'longarm-hub' -eq $Name) { $acr = $r.name; break }
        }
        if (-not $acr) {
            $acr = 'lahub' + [guid]::NewGuid().ToString('N').Substring(0, 12)
            Write-Host "creating the container registry $acr in $rg (Basic, about 5 USD/month)"
            Invoke-Az acr create -g $rg -n $acr -l $Location --sku Basic --admin-enabled true --tags "longarm-hub=$Name" -o none | Out-Null
        }
        $loginServer = (Invoke-Az acr show -g $rg -n $acr --query loginServer -o tsv) -join ''
        $version = 'dev'
        if (Get-Command git -ErrorAction SilentlyContinue) {
            $v = (& { $ErrorActionPreference = 'Continue'; git -C $root describe --tags --always --dirty 2>$null }) -join ''
            if ($v) { $version = $v }
        }
        $tag = 'build-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddHHmmss')
        Write-Host "building $loginServer/longarm-hub:$tag from $root (az acr build, a few minutes)"
        # --target acr: ACR Tasks can't parse the BuildKit stages (deploy/hub/Dockerfile)
        & $script:AzCli acr build -g $rg -r $acr -t "longarm-hub:$tag" --target acr --platform linux/amd64 --build-arg "VERSION=$version" -f deploy/hub/Dockerfile $root
        if ($LASTEXITCODE -ne 0) { throw 'az acr build failed (the build log is above)' }
        $Image = "$loginServer/longarm-hub:$tag"
    }
    # an image in an Azure Container Registry of this resource group: its
    # admin login (no -RegistryUser needed, also on re-runs)
    $regHost = Get-RegistryHost $Image
    if (-not $RegistryUser -and $regHost -like '*.azurecr.io') {
        if (-not $acr) { $acr = (Invoke-AzQuiet acr list -g $rg --query "[?loginServer=='$regHost'].name | [0]" -o tsv) -join '' }
        if ($acr) {
            $cred = @(((Invoke-Az acr credential show -n $acr -g $rg --query '[username, passwords[0].value]' -o tsv) -join '').Trim("`r", "`n") -split "`t") + @('', '')
            if (-not $cred[0] -or -not $cred[1]) { throw "no admin credentials for the registry $acr (az acr update -n $acr --admin-enabled true)" }
            $RegistryUser, $registryPassword = $cred[0], $cred[1]
        }
    }
    if ($curRegistry -and -not $RegistryUser) { throw "the hub pulls its image with a login for $curRegistry, which can't be read back: pass -RegistryUser again (password in `$env:LONGARM_REGISTRY_PASSWORD)" }

    if ($Recreate -and $curImage) {
        Write-Host "deleting container group $Name (storage and data stay)"
        Invoke-Az container delete -g $rg -n $Name --yes -o none | Out-Null
    }

    # parameters in a private file: no secrets on the command line. The
    # location is the resource group's (its programmatic name).
    $p = [ordered]@{
        name               = @{ value = $Name }
        size               = @{ value = $Size }
        enablePanel        = @{ value = $panelOn }
        enableLogAnalytics = @{ value = $logsOn }
    }
    if ($Image) { $p.image = @{ value = $Image } }
    if ($AcmeEmail) { $p.acmeEmail = @{ value = $AcmeEmail } }
    if ($RegistryUser) {
        if (-not $registryPassword) { throw '-RegistryUser needs $env:LONGARM_REGISTRY_PASSWORD' }
        # the login is for the registry the image comes from (the template defaults to ghcr.io)
        if ($regHost) { $p.registryServer = @{ value = $regHost } }
        $p.registryUsername = @{ value = $RegistryUser }
        $p.registryPassword = @{ value = $registryPassword }
    }
    if ($StunServers) {
        $s = $StunServers
        if ($s -eq 'none') { $s = '' }
        $p.stunServers = @{ value = $s }
    }
    if ($TailscaleSecretFile) {
        $p.tailscaleOAuthSecret = @{ value = (Get-Content -Raw $TailscaleSecretFile).Trim() }
    }
    $paramsFile = Join-Path $tmp 'params.json'
    $doc = @{ contentVersion = '1.0.0.0'; parameters = $p }
    [System.IO.File]::WriteAllText($paramsFile, ($doc | ConvertTo-Json -Depth 5))

    Write-Host "deploying $Name to $rg ($Location, $Size, panel $panelOn, $Image); this takes a few minutes"
    $query = '[properties.outputs.hubAddress.value, properties.outputs.fqdn.value, properties.outputs.storageAccount.value, properties.outputs.relayUdpPort.value, properties.outputs.panelUrl.value]'
    # one tab-separated line; panelUrl last (empty without the panel)
    $start = Get-Date
    $deadline = $start.AddSeconds($TimeoutSeconds)
    $logsHint = "az container logs -g $rg -n $Name --container-name hub"
    # --no-wait + polling: a container group stuck in Creating would block
    # "az deployment group create" for an hour (Test-Stuck)
    $deployment = 'longarm-hub-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddHHmmss')
    Invoke-Az deployment group create -g $rg -n $deployment --template-file $template --parameters "@$paramsFile" --no-wait -o none | Out-Null
    $dstate = ''
    while ($dstate -ne 'Succeeded') {
        Start-Sleep -Seconds $script:Poll
        $dstate = (Invoke-AzQuiet deployment group show -g $rg -n $deployment --query properties.provisioningState -o tsv) -join ''
        if ($dstate -eq 'Failed' -or $dstate -eq 'Canceled') {
            $ops = Invoke-AzQuiet deployment operation group list -g $rg -n $deployment --query "[?properties.provisioningState=='Failed'].{resource: properties.targetResource.resourceName, error: properties.statusMessage.error}" -o json
            Write-Warning ($ops -join "`n")
            throw "deployment ${deployment}: $dstate (errors above; az deployment group show -g $rg -n $deployment)"
        }
        Test-Stuck
        if ((Get-Date) -gt $deadline) { throw "deployment $deployment is still $dstate after $TimeoutSeconds s: az deployment group show -g $rg -n $deployment" }
    }
    $outLine = (Invoke-Az deployment group show -g $rg -n $deployment --query $query -o tsv) -join ''
    $outs = @($outLine.Trim("`r", "`n") -split "`t") + @('', '', '', '', '')
    $hubAddr, $fqdn, $account, $relayPort, $panelUrl = $outs[0], $outs[1], $outs[2], $outs[3], $outs[4]
    if (-not $hubAddr -or -not $fqdn -or -not $account) { throw "deployment returned no outputs: $outLine" }
    if (-not $relayPort) { $relayPort = '7443' }
    # the relay's TLS fallback is on the hub's TCP port (ACI: UDP 7445, TCP 7443)
    $hubPort = $hubAddr.Substring($hubAddr.LastIndexOf(':') + 1)
    $relayTcp = ''
    if ($relayPort -ne $hubPort) { $relayTcp = "tcp=${fqdn}:$hubPort&" }
    $key = Invoke-Az storage account keys list -g $rg -n $account --query '[0].value' -o tsv

    $hubImageQuery = "containers[?name=='hub'].image"
    $newImage = Invoke-Az container show -g $rg -n $Name --query $hubImageQuery -o tsv
    if (-not $Recreate -and $curImage -and $curImage -eq $newImage -and $newImage -match ':(latest|edge)$') {
        Write-Host "pulling $newImage again (az container restart)"
        Invoke-Az container restart -g $rg -n $Name -o none | Out-Null
    }

    Write-Host "waiting for the hub at $hubAddr"
    $start = Get-Date
    while ($true) {
        $state = Invoke-AzQuiet container show -g $rg -n $Name --query "containers[?name=='hub'].instanceView.currentState.state" -o tsv
        if ($state -eq 'Running') { break }
        Test-Stuck
        if ((Get-Date) -gt $deadline) { throw "the hub container is not running ($state): $logsHint" }
        Start-Sleep -Seconds $script:Poll
    }
    # bootstrap.env of this start (a new boot_id; restart once if the update
    # didn't restart the hub)
    $restarted = $false
    while ($true) {
        $boot = Get-Bootstrap $account $key $bootFile
        if ($boot -and (Get-Value $boot 'boot_id') -ne $prevBoot) { break }
        if ((Get-Date) -gt $deadline) { throw "no bootstrap.env from this start on the share: $logsHint" }
        if ($prevBoot -and -not $restarted -and ((Get-Date) - $start).TotalSeconds -gt 240) {
            Write-Host 'the hub did not restart: az container restart'
            Invoke-Az container restart -g $rg -n $Name -o none | Out-Null
            $restarted = $true
        }
        Start-Sleep -Seconds 5
    }
    $port = [int]($hubAddr.Substring($hubAddr.LastIndexOf(':') + 1))
    while (-not (Test-TcpPort $fqdn $port)) {
        if ((Get-Date) -gt $deadline) { throw "$hubAddr does not answer: $logsHint" }
        Start-Sleep -Seconds 5
    }
    $fp = Get-Value $boot 'ca_fingerprint'
    $code = Get-Value $boot 'admin_join_code'
    $expires = Get-Value $boot 'admin_join_code_expires'

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
if ($panelOn) { $cpu += 0.25; $mem += 0.3 }
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
   Through the hub's relay (workers behind NAT): longarmd run --listen "longnet://${fqdn}:${relayPort}?${relayTcp}name=worker-1"
"@
if ($panelUrl) {
    Write-Host @"
   Or, once a release is on the hub (longarm admin release publish, docs/usage.md):
     curl -fsSL $panelUrl/install.sh | sh -s -- --join <worker-code>                      # Linux/macOS
     `$env:LONGARM_JOIN='<worker-code>'; irm $panelUrl/install.ps1 | iex                   # Windows
   Panel login: az container exec -g $rg -n $Name --container-name hub --exec-command /usr/local/bin/longarm-hub-user-add
"@
}
Write-Host @"

3. Your agents:
     claude mcp add longarm -- longarm mcp

Upgrade: ./deploy.ps1 -Name $Name -Image <image:tag>    Remove: ./deploy.ps1 -Name $Name -Teardown
"@
