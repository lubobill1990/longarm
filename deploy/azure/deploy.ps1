<#
.SYNOPSIS
  longarm-hub on Azure Container Instances in one command (deploy/azure/README.md).

.DESCRIPTION
  The PowerShell twin of deploy.sh: creates the resource group, deploys
  azuredeploy.json (storage, Litestream replica container, state share,
  the container group), waits for the hub and prints what to run on the
  admin's laptop and on workers. Re-running is an upgrade: the running
  hub's settings (image, size, panel, storage auth, telemetry, Entra
  sign-in, ...) are kept unless a parameter changes them, the data stays,
  the hub restarts.
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
.EXAMPLE
  ./deploy.ps1 -Name myhub -Location eastasia -StorageAuth identity -Telemetry -Diagnostics   # no storage account keys
.EXAMPLE
  ./deploy.ps1 -Name myhub -Headscale -Recreate   # + a Headscale sidecar (self-hosted tailnet)
#>
[CmdletBinding()]
param(
    # hub name = DNS label: NAME.REGION.azurecontainer.io (lowercase: Options None = case-sensitive)
    [Parameter(Mandatory = $true)][ValidatePattern('^[a-z][a-z0-9-]{1,38}[a-z0-9]$', Options = 'None')][string]$Name,
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
    # a self-hosted Tailscale control plane (Headscale) as a sidecar, served by the hub on
    # https://<fqdn>; the hub's tailnet provider (needs the panel; switching needs -Recreate)
    [switch]$Headscale,
    [switch]$NoHeadscale,
    # also hand nodes Tailscale's public DERP relays: on | off (default off)
    [string]$HeadscalePublicDerp = '',
    [string]$AcmeEmail = '',
    # Sign in with Microsoft Entra ID (longarm login, panel): the app registration (docs/usage.md)
    [string]$EntraTenant = '',
    [string]$EntraClient = '',
    # Entra admins: UPNs, *@domain or object IDs, comma-separated
    [string]$EntraAdmins = '',
    # panel sign-in: spa (default) | confidential | off
    [string]$EntraWeb = '',
    # app-only sign-in (CI): "<app id>=<user>", comma-separated
    [string]$EntraApps = '',
    # scheduler memory gate: free GB a worker needs for scheduled jobs (default 20; lower it for small workers)
    [string]$GateFreeGb = '',
    # the minisign public key line (RW...) releases are signed with: the hub refuses uploads whose
    # signature doesn't verify, e.g. -ReleasePublicKey (Get-Content longarm-release.pub)[-1]
    [string]$ReleasePublicKey = '',
    [switch]$LogAnalytics,
    # key (default): storage account keys; identity: a managed identity, shared key access off
    [ValidateSet('', 'key', 'identity')][string]$StorageAuth = '',
    # with identity: pull the image from this registry in the resource group
    [string]$Acr = '',
    # Application Insights + Log Analytics for the fleet's telemetry (docs/telemetry.md)
    [switch]$Telemetry,
    # a blob container for uploaded diagnostic bundles (identity only; 30 days)
    [switch]$Diagnostics,
    [string]$Subscription = '',
    # delete the container group first (to change -Size, the panel or -Headscale): validated first,
    # the settings saved in the resource group's tags, rolled back if the new configuration fails
    [switch]$Recreate,
    # don't check that every image can be pulled before deploying
    [switch]$SkipImageCheck,
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

function Split-Tsv([string]$Line, [int]$Count) {
    # the fields of one az -o tsv line, at least $Count of them; the real az
    # prints None for a null field in a list (a hub without registry
    # credentials): that is an empty field
    $f = @($Line.Trim("`r", "`n") -split "`t" | ForEach-Object { if ($_ -ceq 'None') { '' } else { $_ } })
    while ($f.Count -lt $Count) { $f += '' }
    , $f
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

function Get-BootstrapLog {
    # the last LONGARM_BOOTSTRAP line of the hub's log as a hashtable ($null
    # if none; storage-auth identity: no share; the line has no join code)
    $out = Invoke-AzQuiet container logs -g $rg -n $Name --container-name hub
    if ($LASTEXITCODE -ne 0) { return $null }
    $last = $null
    foreach ($l in (($out -join "`n") -split "`r?`n")) {
        if ($l.StartsWith('LONGARM_BOOTSTRAP ')) { $last = $l }
    }
    if (-not $last) { return $null }
    $kv = @{}
    foreach ($w in $last.Split(' ')) {
        $i = $w.IndexOf('=')
        if ($i -gt 0) { $kv[$w.Substring(0, $i)] = $w.Substring($i + 1) }
    }
    if ($kv.Count -eq 0) { return $null }
    $kv
}

function Read-Bootstrap([string]$Account, [string]$Key, [string]$Dest) {
    # this hub's bootstrap values: the share (key) or the log (identity)
    if ($StorageAuth -eq 'identity') { return Get-BootstrapLog }
    Get-Bootstrap $Account $Key $Dest
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
        $shown = if ($state) { $state } else { 'unknown' }
        Write-Host "registering the resource provider $ns ($shown; once per subscription, a minute or two)"
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
    # the message (else $null) when the container group has sat in Creating with waiting
    # containers and no events at all for -StuckTimeoutSeconds (seen once on
    # a real run: 30+ min, while a recreate came up in 3.5 min)
    $q = "[provisioningState, containers[?name=='hub'].instanceView.currentState.state | [0], length(containers[].instanceView.events[] || ``[]``), length(instanceView.events || ``[]``)]"
    $line = (Invoke-AzQuiet container show -g $rg -n $Name -o tsv --query $q) -join ''
    if ($LASTEXITCODE -ne 0 -or -not $line) { return }
    $f = Split-Tsv $line 4
    $events = 0
    foreach ($x in $f[2], $f[3]) { if ($x -match '^\d+$') { $events += [int]$x } }
    if (($f[0] -eq 'Creating' -or $f[0] -eq 'Pending') -and ($f[1] -eq '' -or $f[1] -eq 'Waiting') -and $events -eq 0) {
        if (-not $script:StuckSince) { $script:StuckSince = Get-Date }
        $secs = [int]((Get-Date) - $script:StuckSince).TotalSeconds
        if ($secs -ge $script:StuckTimeout) {
            return @"
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
if ($NoHeadscale -and $Headscale) { throw '-NoHeadscale and -Headscale exclude each other' }
if ($HeadscalePublicDerp -and $HeadscalePublicDerp -cnotmatch '^(on|off)$') { throw '-HeadscalePublicDerp: on or off' }
if ($TailscaleSecretFile -and -not (Test-Path $TailscaleSecretFile)) { throw "-TailscaleSecretFile: can't read $TailscaleSecretFile" }
if (-not $ResourceGroup) { $ResourceGroup = "$Name-rg" }
if ($BuildInAcr -and $Image) { throw '-BuildInAcr builds the image: drop -Image' }
# none alone, or host:port / [v6]:port entries (the hub refuses a missing port: the container would crash-loop)
if ($StunServers -and $StunServers -ne 'none') {
    foreach ($s in $StunServers.Split(',')) {
        if ($s -notmatch '^([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\]):[0-9]{1,5}$') { throw "-StunServers: '$s' is not host:port or [v6]:port (none goes alone)" }
    }
}
if ($GateFreeGb -and $GateFreeGb -notmatch '^[0-9]+([.][0-9]+)?$') { throw '-GateFreeGb: a number of GB' }
# the key line of a minisign .pub file, not the file or its comment line
if ($ReleasePublicKey -and $ReleasePublicKey -cnotmatch '^RW[A-Za-z0-9+/=]{40,}$') { throw '-ReleasePublicKey: the minisign public key line (RW...), e.g. (Get-Content longarm-release.pub)[-1]' }
$StorageAuth = $StorageAuth.ToLowerInvariant()
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
    $telemetryOn = [bool]$Telemetry
    $diagnosticsOn = [bool]$Diagnostics
    $headscaleOn = $null
    if ($NoHeadscale) { $headscaleOn = $false }
    if ($Headscale) { $headscaleOn = $true }
    $publicDerpOn = $null
    if ($HeadscalePublicDerp) { $publicDerpOn = ($HeadscalePublicDerp -eq 'on') }

    # A re-run keeps the hub's settings ($prev). Their source, in this order:
    #   1. the container group (the running hub's image and environment)
    #   2. the snapshot this script tags the resource group with before it
    #      deletes the group (-Recreate), until a deployment succeeds again
    #   3. the parameters of the last successful deployment (ARM keeps them)
    # Without any of them, a resource group that has a hub's storage needs the
    # settings given explicitly: defaulting would switch storage auth or drop
    # the identity (seen on a real run: a retry after a failed recreate fell
    # back to storage keys, which the subscription's policy denies). Secrets
    # can't be read back from any of them: a hub that has them needs them again.
    $prevKeys = 'image', 'size', 'panel', 'auth', 'acr', 'logs', 'telemetry', 'diagnostics', 'stun', 'gate', 'relkey',
        'entra_tenant', 'entra_client', 'entra_admins', 'entra_web', 'entra_apps', 'headscale', 'public_derp', 'registry', 'registry_user', 'tailscale'
    function Get-EmptyPrev { $h = [ordered]@{}; foreach ($k in $prevKeys) { $h[$k] = '' }; $h }
    function ConvertTo-Flag($b) { if ($b) { 'true' } else { 'false' } }

    function Get-PrevGroup {
        $q = "[containers[0].image, containers[0].resources.requests.cpu, containers[1].name, imageRegistryCredentials[0].server, join(',', containers[0].environmentVariables[].name), diagnostics.logAnalytics.workspaceId, containers[0].environmentVariables[?name=='LONGARM_HUB_RELAY_STUN_ADVERTISE'].value | [0]]"
        $line = (Invoke-AzQuiet container show -g $rg -n $Name -o tsv --query $q) -join ''
        if ($LASTEXITCODE -ne 0 -or -not $line) { return $null }
        $f = Split-Tsv $line 7
        $p = Get-EmptyPrev
        $p.image = $f[0]
        $p.size = if ([double]$f[1] -ge 2) { 'medium' } else { 'small' }
        $p.registry = $f[3]
        $names = ",$($f[4]),"
        $p.auth = if ($names -like '*,LONGARM_HUB_CA_IN_DB,*') { 'identity' } else { 'key' }
        # the panel: Caddy (storage-auth key) or the hub on 443 (identity)
        $p.panel = ConvertTo-Flag (($f[2] -eq 'caddy') -or ($names -like '*,LONGARM_HUB_HTTPS,*'))
        # the registry pulled with the identity (identity mode)
        if ($p.auth -eq 'identity' -and $p.registry -like '*.azurecr.io') { $p.acr = $p.registry.Substring(0, $p.registry.IndexOf('.')) }
        $p.logs = ConvertTo-Flag ([bool]$f[5])
        $p.telemetry = ConvertTo-Flag ($names -like '*,LONGARM_HUB_APPINSIGHTS_CONNECTION_STRING,*')
        $p.diagnostics = ConvertTo-Flag ($names -like '*,LONGARM_HUB_DIAG_BLOB_URL,*')
        $p.headscale = ConvertTo-Flag ($names -like '*,LONGARM_HUB_HEADSCALE_DIR,*')
        $p.public_derp = ConvertTo-Flag ($names -like '*,LONGARM_HUB_HEADSCALE_PUBLIC_DERP,*')
        $p.tailscale = ConvertTo-Flag ($names -like '*,LONGARM_HUB_TAILSCALE_OAUTH_SECRET,*')
        # the template sets self[,<stunServers>]; absent = a hub from before (template default)
        if ($f[6] -eq 'self') { $p.stun = 'none' } elseif ($f[6] -like 'self,*') { $p.stun = $f[6].Substring(5) }
        # plain environment values: the scheduler gate, the release key, Entra
        $vq = "containers[0].environmentVariables[?starts_with(name, 'LONGARM_HUB_ENTRA_') || name=='LONGARM_HUB_SCHED_GATE_FREE_GB' || name=='LONGARM_HUB_RELEASES_PUBLIC_KEY'].[name, value]"
        foreach ($l in @(Invoke-AzQuiet container show -g $rg -n $Name -o tsv --query $vq)) {
            $kv = "$l".TrimEnd("`r", "`n") -split "`t", 2
            if ($kv.Count -lt 2) { continue }
            $v = if ($kv[1] -ceq 'None') { '' } else { $kv[1] }
            switch -CaseSensitive ($kv[0]) {
                'LONGARM_HUB_SCHED_GATE_FREE_GB' { $p.gate = $v }
                'LONGARM_HUB_RELEASES_PUBLIC_KEY' { $p.relkey = $v }
                'LONGARM_HUB_ENTRA_TENANT_ID' { $p.entra_tenant = $v }
                'LONGARM_HUB_ENTRA_CLIENT_ID' { $p.entra_client = $v }
                'LONGARM_HUB_ENTRA_ADMINS' { $p.entra_admins = $v }
                'LONGARM_HUB_ENTRA_WEB' { $p.entra_web = $v }
                'LONGARM_HUB_ENTRA_APPS' { $p.entra_apps = $v }
            }
        }
        $p
    }

    # The snapshot (the same format as deploy.sh's): key=value lines, base64,
    # in resource group tags of 250 characters (longarm-hub-prev-0..N-1;
    # longarm-hub-prev-n = N, 0 = none).
    function Get-RgId { (Invoke-Az group show -n $rg --query id -o tsv) -join '' }
    function Save-Snapshot($p) {
        $text = ($prevKeys | ForEach-Object { "$_=$($p[$_])`n" }) -join ''
        $b64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($text))
        $n = [int][math]::Ceiling($b64.Length / 250)
        $tags = @()
        for ($i = 0; $i -lt $n; $i++) { $tags += "longarm-hub-prev-$i=" + $b64.Substring($i * 250, [math]::Min(250, $b64.Length - $i * 250)) }
        $tags += "longarm-hub-prev-n=$n"
        $tags += 'longarm-hub-prev-at=' + (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
        Invoke-Az tag update --resource-id (Get-RgId) --operation Merge --tags @tags -o none | Out-Null
    }
    function Clear-Snapshot {
        Invoke-AzQuiet tag update --resource-id (Get-RgId) --operation Merge --tags 'longarm-hub-prev-n=0' -o none | Out-Null
        if ($LASTEXITCODE -ne 0) { Write-Warning 'the settings snapshot (resource group tag longarm-hub-prev-n) was not cleared' }
    }
    function Get-PrevSnapshot {
        $tags = ((Invoke-AzQuiet group show -n $rg --query tags -o json) -join "`n") | ConvertFrom-Json
        if (-not $tags -or -not $tags.PSObject.Properties['longarm-hub-prev-n']) { return $null }
        $n = 0
        if (-not [int]::TryParse([string]$tags.'longarm-hub-prev-n', [ref]$n) -or $n -le 0) { return $null }
        $b64 = ''
        for ($i = 0; $i -lt $n; $i++) { $b64 += [string]$tags."longarm-hub-prev-$i" }
        try { $text = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($b64)) } catch { return $null }
        $p = Get-EmptyPrev
        foreach ($l in ($text -split "`n")) {
            $i = $l.IndexOf('=')
            if ($i -gt 0 -and $p.Contains($l.Substring(0, $i))) { $p[$l.Substring(0, $i)] = $l.Substring($i + 1) }
        }
        if ($p.auth -ne 'key' -and $p.auth -ne 'identity') { return $null }
        $p
    }

    # the parameters of the last successful deployment of this script (absent
    # parameters: the template's defaults)
    function Get-PrevDeployment {
        $deps = @(Invoke-AzQuiet deployment group list -g $rg -o tsv --query "[?properties.provisioningState=='Succeeded' && starts_with(name, 'longarm-hub-')].name" |
            ForEach-Object { "$_".Trim() } | Where-Object { $_ } | Sort-Object)
        if ($deps.Count -eq 0) { return $null }
        $dep = $deps[-1]
        $list = 'image', 'size', 'enablePanel', 'storageAuth', 'acrName', 'enableLogAnalytics', 'enableTelemetry', 'enableDiagnostics', 'stunServers',
            'schedulerGateFreeGb', 'releasePublicKey', 'entraTenantId', 'entraClientId', 'entraAdmins', 'entraWeb', 'entraApps', 'enableHeadscale',
            'headscalePublicDerp', 'registryServer', 'registryUsername'
        $q = '[' + (($list | ForEach-Object { "properties.parameters.$_.value" }) -join ', ') + ', properties.parameters.stunServers == null]'
        $line = (Invoke-AzQuiet deployment group show -g $rg -n $dep -o tsv --query $q) -join ''
        if ($LASTEXITCODE -ne 0 -or -not $line) { return $null }
        $f = Split-Tsv $line ($list.Count + 1)
        $b = { param($x) ConvertTo-Flag ($x -eq 'true') }
        $p = Get-EmptyPrev
        $p.image = $f[0]; $p.size = if ($f[1]) { $f[1] } else { 'small' }; $p.panel = & $b $f[2]; $p.auth = $f[3]; $p.acr = $f[4]
        $p.logs = & $b $f[5]; $p.telemetry = & $b $f[6]; $p.diagnostics = & $b $f[7]; $p.stun = $f[8]; $p.gate = $f[9]; $p.relkey = $f[10]
        $p.entra_tenant = $f[11]; $p.entra_client = $f[12]; $p.entra_admins = $f[13]; $p.entra_web = $f[14]; $p.entra_apps = $f[15]
        $p.headscale = & $b $f[16]; $p.public_derp = & $b $f[17]; $p.registry = $f[18]; $p.registry_user = $f[19]
        # stunServers: absent = the template's default, "" = none
        if ($f[20] -eq 'false' -and -not $p.stun) { $p.stun = 'none' }
        if ($p.auth -ne 'key' -and $p.auth -ne 'identity') { return $null }
        $p.Add('source', "deployment $dep")
        $p
    }

    $prevSrc = ''
    $prev = Get-PrevGroup
    if ($prev) {
        $prevSrc = 'group'
    } else {
        $prev = Get-PrevSnapshot
        if ($prev) {
            $prevSrc = 'snapshot'
            Write-Host "the container group $Name is missing: its settings come from the snapshot taken before it was deleted"
        } else {
            $prev = Get-PrevDeployment
            if ($prev) {
                $prevSrc = 'deployment'
                Write-Host "the container group $Name is missing: its settings come from the last successful deployment ($($prev.source.Substring(11)))"
            } elseif (-not $StorageAuth -and (@(Invoke-AzQuiet storage account list -g $rg --query "[?starts_with(name, 'lahub')].name" -o tsv) -join '')) {
                throw "the resource group $rg has a hub's storage, but neither a container group, a settings snapshot nor a successful deployment to read its settings from: pass them (at least -StorageAuth key|identity; and -Image, -Entra*, -Telemetry, -Diagnostics, -Headscale, -ReleasePublicKey, -GateFreeGb, -StunServers as the hub had them)"
            }
        }
    }

    $curImage = ''
    $prevBoot = ''
    $account = ''
    $key = ''
    if ($prev) {
        if ($StorageAuth -and $StorageAuth -ne $prev.auth) { throw "the hub uses -StorageAuth $($prev.auth); switching needs a new deployment (another -Name or -ResourceGroup)" }
        $StorageAuth = $prev.auth
        if (-not $Acr) { $Acr = $prev.acr }
        if ($prevSrc -eq 'group') {
            $curImage = $prev.image
            if (-not $Recreate) {
                if ($Size -and $Size -ne $prev.size) { throw "the hub is $($prev.size); ACI can't resize a running container group: add -Recreate (data is kept)" }
                $t = if ($prev.panel -eq 'true') { 'on' } else { 'off' }
                if ($null -ne $panelOn -and (ConvertTo-Flag $panelOn) -ne $prev.panel) { throw "the panel is $t; ACI can't change the ports of a running group: add -Recreate (data is kept)" }
                $t = if ($prev.headscale -eq 'true') { 'on' } else { 'off' }
                if ($null -ne $headscaleOn -and (ConvertTo-Flag $headscaleOn) -ne $prev.headscale) { throw "the Headscale sidecar is $t; ACI can't add or remove containers of a running group: add -Recreate (data is kept)" }
            }
        }
        if (-not $Image) { $Image = $prev.image }
        if (-not $Size) { $Size = $prev.size }
        if ($null -eq $panelOn) { $panelOn = $prev.panel -eq 'true' }
        if ($null -eq $headscaleOn) { $headscaleOn = $prev.headscale -eq 'true' }
        if ($null -eq $publicDerpOn) { $publicDerpOn = $prev.public_derp -eq 'true' }
        if ($prev.logs -eq 'true') { $logsOn = $true }
        if ($prev.telemetry -eq 'true') { $telemetryOn = $true }
        if ($prev.diagnostics -eq 'true') { $diagnosticsOn = $true }
        if (-not $StunServers) { $StunServers = $prev.stun }
        if (-not $GateFreeGb) { $GateFreeGb = $prev.gate }
        if (-not $ReleasePublicKey) { $ReleasePublicKey = $prev.relkey }
        if (-not $EntraTenant) {
            $EntraTenant = $prev.entra_tenant
            if (-not $EntraClient) { $EntraClient = $prev.entra_client }
            if (-not $EntraAdmins) { $EntraAdmins = $prev.entra_admins }
            if (-not $EntraWeb) { $EntraWeb = $prev.entra_web }
            if (-not $EntraApps) { $EntraApps = $prev.entra_apps }
        }
        if ($prev.tailscale -eq 'true' -and -not $TailscaleSecretFile -and -not $headscaleOn) { throw "the hub has a Tailscale OAuth secret, which can't be read back: pass -TailscaleSecretFile again" }
        # this start's bootstrap values must differ from the running one's (boot_id)
        if ($prevSrc -eq 'group') {
            if ($StorageAuth -eq 'identity') {
                $prevBoot = Get-Value (Get-BootstrapLog) 'boot_id'
            } else {
                $account = @(Invoke-Az storage account list -g $rg --query "[?starts_with(name, 'lahub')].name" -o tsv) | Select-Object -First 1
                if ($account) {
                    $key = (Invoke-Az storage account keys list -g $rg -n $account --query '[0].value' -o tsv) -join ''
                    $prevBoot = Get-Value (Get-Bootstrap $account $key $bootFile) 'boot_id'
                }
            }
        }
    }
    if (-not $StorageAuth) { $StorageAuth = 'key' }
    if ($diagnosticsOn -and $StorageAuth -ne 'identity') { throw '-Diagnostics needs -StorageAuth identity (the hub writes bundles with its managed identity)' }
    if ($GateFreeGb -and $GateFreeGb -notmatch '^[0-9]+([.][0-9]+)?$') { throw '-GateFreeGb: a number of GB' }
    if ([bool]$EntraTenant -ne [bool]$EntraClient) { throw '-EntraTenant and -EntraClient go together' }
    if ($EntraWeb -and $EntraWeb -cnotmatch '^(spa|confidential|off)$') { throw '-EntraWeb: spa, confidential or off' }
    if ($Acr -and $StorageAuth -ne 'identity') { throw '-Acr needs -StorageAuth identity' }
    if (-not $Size) { $Size = 'small' }
    if ($null -eq $panelOn) { $panelOn = $true }
    $headscaleOn = [bool]$headscaleOn
    $publicDerpOn = [bool]$publicDerpOn
    if ($headscaleOn -and -not $panelOn) { throw '-Headscale needs the panel (Headscale is served on https://<fqdn>): drop -NoPanel' }
    if ($headscaleOn -and $TailscaleSecretFile) { throw '-Headscale and -TailscaleSecretFile: one tailnet provider (the sidecar is the provider)' }
    if ($logsOn) { Register-Provider 'Microsoft.OperationalInsights' }
    # Application Insights on its Log Analytics workspace
    if ($telemetryOn) { Register-Provider 'Microsoft.OperationalInsights', 'Microsoft.Insights' }

    $buildAcr = ''
    $registryPassword = $env:LONGARM_REGISTRY_PASSWORD
    if ($BuildInAcr) {
        # the image from this checkout, built by ACR Tasks (no docker here); a
        # registry in the hub's resource group, created on the first use
        Register-Provider 'Microsoft.ContainerRegistry'
        $root = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
        if (-not (Test-Path (Join-Path $root 'deploy/hub/Dockerfile')) -or -not (Test-Path (Join-Path $root 'go.mod'))) { throw '-BuildInAcr: run from a checkout of the repository' }
        # filtered here: an embedded " in --query breaks with pwsh 7.3+ native argument passing
        $regs = ((Invoke-Az acr list -g $rg -o json) -join "`n") | ConvertFrom-Json
        foreach ($r in @($regs)) {
            if ($null -ne $r -and $r.PSObject.Properties['tags'] -and $r.tags -and $r.tags.PSObject.Properties['longarm-hub'] -and $r.tags.'longarm-hub' -eq $Name) { $buildAcr = $r.name; break }
        }
        if (-not $buildAcr) {
            $buildAcr = 'lahub' + [guid]::NewGuid().ToString('N').Substring(0, 12)
            Write-Host "creating the container registry $buildAcr in $rg (Basic, about 5 USD/month)"
            # key: the hub pulls with the admin login; identity: with its managed identity (AcrPull)
            $adminEnabled = if ($StorageAuth -eq 'key') { 'true' } else { 'false' }
            Invoke-Az acr create -g $rg -n $buildAcr -l $Location --sku Basic --admin-enabled $adminEnabled --tags "longarm-hub=$Name" -o none | Out-Null
        }
        $loginServer = (Invoke-Az acr show -g $rg -n $buildAcr --query loginServer -o tsv) -join ''
        $version = 'dev'
        if (Get-Command git -ErrorAction SilentlyContinue) {
            $v = (& { $ErrorActionPreference = 'Continue'; git -C $root describe --tags --always --dirty 2>$null }) -join ''
            if ($v) { $version = $v }
        }
        $tag = 'build-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddHHmmss')
        Write-Host "building $loginServer/longarm-hub:$tag from $root (az acr build, a few minutes)"
        # --target acr: ACR Tasks can't parse the BuildKit stages (deploy/hub/Dockerfile)
        & $script:AzCli acr build -g $rg -r $buildAcr -t "longarm-hub:$tag" --target acr --platform linux/amd64 --build-arg "VERSION=$version" -f deploy/hub/Dockerfile $root
        if ($LASTEXITCODE -ne 0) { throw 'az acr build failed (the build log is above)' }
        $Image = "$loginServer/longarm-hub:$tag"
    }
    # an image in an Azure Container Registry of this resource group: key: its
    # admin login, identity: the managed identity pulls it (-Acr); no
    # -RegistryUser needed, also on re-runs
    $regHost = Get-RegistryHost $Image
    if (-not $RegistryUser -and $regHost -like '*.azurecr.io') {
        $regAcr = $buildAcr
        if (-not $regAcr) { $regAcr = (Invoke-AzQuiet acr list -g $rg --query "[?loginServer=='$regHost'].name | [0]" -o tsv) -join '' }
        if ($StorageAuth -eq 'identity') {
            if (-not $Acr) { $Acr = $regAcr }
        } elseif ($regAcr) {
            $cred = Split-Tsv ((Invoke-Az acr credential show -n $regAcr -g $rg --query '[username, passwords[0].value]' -o tsv) -join '') 2
            if (-not $cred[0] -or -not $cred[1]) { throw "no admin credentials for the registry $regAcr (az acr update -n $regAcr --admin-enabled true)" }
            $RegistryUser, $registryPassword = $cred[0], $cred[1]
        }
    }
    if ($prev -and $prev.registry -and -not $RegistryUser -and -not $Acr) { throw "the hub pulls its image with a login for $($prev.registry), which can't be read back: pass -RegistryUser again (password in `$env:LONGARM_REGISTRY_PASSWORD)" }

    # parameters in a private file: no secrets on the command line. The
    # location is the resource group's (its programmatic name). $cfg: this
    # run's settings; the rollback's are a copy with the previous ones.
    $cfg = @{
        Name = $Name; Size = $Size; Panel = [bool]$panelOn; Logs = [bool]$logsOn; StorageAuth = $StorageAuth
        Telemetry = [bool]$telemetryOn; Diagnostics = [bool]$diagnosticsOn; Headscale = [bool]$headscaleOn; PublicDerp = [bool]$publicDerpOn
        Acr = $Acr; Image = $Image; AcmeEmail = $AcmeEmail; Gate = $GateFreeGb; RelKey = $ReleasePublicKey
        EntraTenant = $EntraTenant; EntraClient = $EntraClient; EntraAdmins = $EntraAdmins; EntraWeb = $EntraWeb; EntraApps = $EntraApps
        RegistryUser = $RegistryUser; RegistryPassword = $registryPassword; RegHost = $regHost; Stun = $StunServers; TailscaleSecretFile = $TailscaleSecretFile
    }
    function Write-ParamFile($c, [string]$File) {
        $p = [ordered]@{
            name                = @{ value = $c.Name }
            size                = @{ value = $c.Size }
            enablePanel         = @{ value = [bool]$c.Panel }
            enableLogAnalytics  = @{ value = [bool]$c.Logs }
            storageAuth         = @{ value = $c.StorageAuth }
            enableTelemetry     = @{ value = [bool]$c.Telemetry }
            enableDiagnostics   = @{ value = [bool]$c.Diagnostics }
            enableHeadscale     = @{ value = [bool]$c.Headscale }
            headscalePublicDerp = @{ value = [bool]$c.PublicDerp }
        }
        if ($c.Acr) { $p.acrName = @{ value = $c.Acr } }
        if ($c.Image) { $p.image = @{ value = $c.Image } }
        if ($c.AcmeEmail) { $p.acmeEmail = @{ value = $c.AcmeEmail } }
        if ($c.Gate) { $p.schedulerGateFreeGb = @{ value = $c.Gate } }
        if ($c.RelKey) { $p.releasePublicKey = @{ value = $c.RelKey } }
        if ($c.EntraTenant) {
            $p.entraTenantId = @{ value = $c.EntraTenant }
            $p.entraClientId = @{ value = $c.EntraClient }
            if ($c.EntraAdmins) { $p.entraAdmins = @{ value = $c.EntraAdmins } }
            if ($c.EntraWeb) { $p.entraWeb = @{ value = $c.EntraWeb } }
            if ($c.EntraApps) { $p.entraApps = @{ value = $c.EntraApps } }
        }
        if ($c.RegistryUser) {
            if (-not $c.RegistryPassword) { throw '-RegistryUser needs $env:LONGARM_REGISTRY_PASSWORD' }
            # the login is for the registry the image comes from (the template defaults to ghcr.io)
            if ($c.RegHost) { $p.registryServer = @{ value = $c.RegHost } }
            $p.registryUsername = @{ value = $c.RegistryUser }
            $p.registryPassword = @{ value = $c.RegistryPassword }
        }
        if ($c.Stun) {
            $s = $c.Stun
            if ($s -eq 'none') { $s = '' }
            $p.stunServers = @{ value = $s }
        }
        if ($c.TailscaleSecretFile) {
            $p.tailscaleOAuthSecret = @{ value = (Get-Content -Raw $c.TailscaleSecretFile).Trim() }
        }
        $doc = @{ contentVersion = '1.0.0.0'; parameters = $p }
        [System.IO.File]::WriteAllText($File, ($doc | ConvertTo-Json -Depth 5))
    }
    $paramsFile = Join-Path $tmp 'params.json'
    Write-ParamFile $cfg $paramsFile

    # Test-ImagePullable: $null when the registry has the image's manifest,
    # asked the way ACI pulls it (anonymously, or with the deployment's
    # registry login; an Azure Container Registry pulled with the managed
    # identity: az acr), else what went wrong. The tests point
    # LONGARM_DEPLOY_REGISTRY_URL at a fake registry.
    function Test-ImagePullable([string]$Ref) {
        $accept = 'application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
        $h = Get-RegistryHost $Ref
        $rest = if ($h) { $Ref.Substring($h.Length + 1) } else { $Ref }
        if (-not $h -or $h -eq 'docker.io') {
            $h = 'registry-1.docker.io'
            if ($rest -notlike '*/*') { $rest = "library/$rest" }
        }
        if ($rest -like '*@*') {
            $repo = $rest.Substring(0, $rest.IndexOf('@'))
            $reference = $rest.Substring($rest.IndexOf('@') + 1)
            $last = $repo.Substring($repo.LastIndexOf('/') + 1)
            if ($last -like '*:*') { $repo = $repo.Substring(0, $repo.LastIndexOf(':')) }
        } else {
            $last = $rest.Substring($rest.LastIndexOf('/') + 1)
            if ($last -like '*:*') {
                $repo = $rest.Substring(0, $rest.LastIndexOf(':'))
                $reference = $rest.Substring($rest.LastIndexOf(':') + 1)
            } else {
                $repo = $rest
                $reference = 'latest'
            }
        }
        if ($h -like '*.azurecr.io' -and $StorageAuth -eq 'identity' -and -not $RegistryUser) {
            $sep = if ($reference -like 'sha256:*') { '@' } else { ':' }
            Invoke-AzQuiet acr repository show -n $h.Substring(0, $h.IndexOf('.')) --image "$repo$sep$reference" -o none | Out-Null
            if ($LASTEXITCODE -eq 0) { return $null }
            return "az acr repository show --image $repo$sep$reference failed"
        }
        $url = "https://$h/v2/$repo/manifests/$reference"
        if ($env:LONGARM_DEPLOY_REGISTRY_URL) { $url = "$($env:LONGARM_DEPLOY_REGISTRY_URL)/$h/v2/$repo/manifests/$reference" }
        Add-Type -AssemblyName System.Net.Http
        $client = New-Object System.Net.Http.HttpClient
        $client.Timeout = [TimeSpan]::FromSeconds(20)
        try {
            $head = {
                param($token)
                $req = New-Object System.Net.Http.HttpRequestMessage([System.Net.Http.HttpMethod]::Head, $url)
                $req.Headers.TryAddWithoutValidation('Accept', $accept) | Out-Null
                if ($token) { $req.Headers.TryAddWithoutValidation('Authorization', "Bearer $token") | Out-Null }
                $client.SendAsync($req).GetAwaiter().GetResult()
            }
            $resp = & $head ''
            if ([int]$resp.StatusCode -eq 401) {
                # a token for repository:<repo>:pull from the realm the registry names
                $ch = @($resp.Headers.WwwAuthenticate | Where-Object { $_.Scheme -eq 'Bearer' }) | Select-Object -First 1
                $par = if ($ch) { [string]$ch.Parameter } else { '' }
                $realm = if ($par -match 'realm="([^"]*)"') { $Matches[1] } else { '' }
                $service = if ($par -match 'service="([^"]*)"') { $Matches[1] } else { '' }
                $scope = if ($par -match 'scope="([^"]*)"') { $Matches[1] } else { "repository:${repo}:pull" }
                if ($realm) {
                    $treq = New-Object System.Net.Http.HttpRequestMessage([System.Net.Http.HttpMethod]::Get, "${realm}?service=$([uri]::EscapeDataString($service))&scope=$([uri]::EscapeDataString($scope))")
                    if ($RegistryUser -and $h -eq $regHost) {
                        $basic = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes("${RegistryUser}:$registryPassword"))
                        $treq.Headers.TryAddWithoutValidation('Authorization', "Basic $basic") | Out-Null
                    }
                    $tresp = $client.SendAsync($treq).GetAwaiter().GetResult()
                    $token = ''
                    if ($tresp.IsSuccessStatusCode) {
                        $tj = $tresp.Content.ReadAsStringAsync().GetAwaiter().GetResult() | ConvertFrom-Json
                        if ($tj.PSObject.Properties['token']) { $token = $tj.token } elseif ($tj.PSObject.Properties['access_token']) { $token = $tj.access_token }
                    }
                    $resp = & $head $token
                }
            }
            if ([int]$resp.StatusCode -eq 200) { return $null }
            return "the registry answered $([int]$resp.StatusCode) for $url"
        } catch {
            return "no answer from $url ($($_.Exception.Message))"
        } finally {
            $client.Dispose()
        }
    }

    # every image of the deployment can be pulled
    function Test-DeploymentImage {
        $defaults = (Get-Content -Raw $template | ConvertFrom-Json).parameters
        $imgs = @($(if ($Image) { $Image } else { $defaults.image.defaultValue }))
        if ($StorageAuth -eq 'key' -and $panelOn) { $imgs += $defaults.caddyImage.defaultValue }
        if ($headscaleOn) { $imgs += $defaults.headscaleImage.defaultValue }
        foreach ($img in $imgs) {
            if (-not $img) { continue }
            $problem = Test-ImagePullable $img
            if ($problem) { throw "the image $img can't be pulled ($problem): nothing was changed. Check the tag, that the package is public (or -RegistryUser), or skip this check with -SkipImageCheck" }
        }
    }

    Write-Host 'validating the deployment (nothing is changed yet)'
    & $script:AzCli deployment group validate -g $rg --template-file $template --parameters "@$paramsFile" -o none | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'the deployment does not validate (the error is above, e.g. a policy violation): nothing was changed' }
    if (-not $SkipImageCheck) { Test-DeploymentImage }

    $script:Deleted = $false
    $rollbackFile = Join-Path $tmp 'rollback.json'
    if ($Recreate -and $curImage) {
        # the previous configuration, for a rollback if the new one fails
        $rcfg = $cfg.Clone()
        $rcfg.Image = $prev.image; $rcfg.Size = $prev.size; $rcfg.Panel = $prev.panel -eq 'true'; $rcfg.Acr = $prev.acr
        $rcfg.Headscale = $prev.headscale -eq 'true'; $rcfg.PublicDerp = $prev.public_derp -eq 'true'; $rcfg.Logs = $prev.logs -eq 'true'
        $rcfg.Telemetry = $prev.telemetry -eq 'true'; $rcfg.Diagnostics = $prev.diagnostics -eq 'true'; $rcfg.Stun = $prev.stun
        $rcfg.Gate = $prev.gate; $rcfg.RelKey = $prev.relkey; $rcfg.EntraTenant = $prev.entra_tenant; $rcfg.EntraClient = $prev.entra_client
        $rcfg.EntraAdmins = $prev.entra_admins; $rcfg.EntraWeb = $prev.entra_web; $rcfg.EntraApps = $prev.entra_apps
        $rcfg.RegHost = Get-RegistryHost $prev.image
        if ($prev.tailscale -ne 'true') { $rcfg.TailscaleSecretFile = '' }
        Write-ParamFile $rcfg $rollbackFile
        & $script:AzCli deployment group validate -g $rg --template-file $template --parameters "@$rollbackFile" -o none | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'the previous configuration (the rollback if the new one fails) does not validate (the error is above): nothing was changed' }
        Write-Host "saving the hub's settings (resource group tags longarm-hub-prev-*: a re-run finds them if the group is gone)"
        Save-Snapshot $prev
        Write-Host "deleting container group $Name (storage and data stay)"
        Invoke-Az container delete -g $rg -n $Name --yes -o none | Out-Null
        $script:Deleted = $true
    }

    # Invoke-DeployRun: deploys and waits for the deployment; $null when it
    # succeeded, else why not. --no-wait + polling: a container group stuck in
    # Creating would block "az deployment group create" for an hour (Test-Stuck)
    function Invoke-DeployRun([string]$File, [string]$Dep) {
        $script:StuckSince = $null
        $end = (Get-Date).AddSeconds($TimeoutSeconds)
        Invoke-AzQuiet deployment group create -g $rg -n $Dep --template-file $template --parameters "@$File" --no-wait -o none | Out-Null
        if ($LASTEXITCODE -ne 0) { return "az deployment group create $Dep failed" }
        $dstate = ''
        while ($dstate -ne 'Succeeded') {
            Start-Sleep -Seconds $script:Poll
            $dstate = (Invoke-AzQuiet deployment group show -g $rg -n $Dep --query properties.provisioningState -o tsv) -join ''
            if ($dstate -eq 'Failed' -or $dstate -eq 'Canceled') {
                $ops = Invoke-AzQuiet deployment operation group list -g $rg -n $Dep --query "[?properties.provisioningState=='Failed'].{resource: properties.targetResource.resourceName, error: properties.statusMessage.error}" -o json
                Write-Warning ($ops -join "`n")
                return "deployment ${Dep}: $dstate (errors above; az deployment group show -g $rg -n $Dep)"
            }
            $stuck = Test-Stuck
            if ($stuck) { return $stuck }
            if ((Get-Date) -gt $end) { return "deployment $Dep is still $dstate after $TimeoutSeconds s: az deployment group show -g $rg -n $Dep" }
        }
        $null
    }

    # Invoke-FailDeploy: the new configuration failed. After -Recreate deleted
    # the group, the previous configuration is deployed again.
    function Invoke-FailDeploy([string]$Why) {
        if (-not $script:Deleted) { throw $Why }
        $script:Deleted = $false
        Write-Warning $Why
        $pimg = if ($prev.image) { $prev.image } else { 'the template default' }
        Write-Warning "rolling back: deploying the previous configuration again (image $pimg, size $($prev.size), panel $($prev.panel), headscale $($prev.headscale))"
        Invoke-AzQuiet container delete -g $rg -n $Name --yes -o none | Out-Null
        $rb = Invoke-DeployRun $rollbackFile ('longarm-hub-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddHHmmss') + '-rollback')
        if (-not $rb) {
            $st = ''
            $end = (Get-Date).AddSeconds($TimeoutSeconds)
            while ($st -ne 'Running' -and (Get-Date) -lt $end) {
                Start-Sleep -Seconds $script:Poll
                $st = (Invoke-AzQuiet container show -g $rg -n $Name --query "containers[?name=='hub'].instanceView.currentState.state" -o tsv) -join ''
            }
            Clear-Snapshot
            throw "$Why`nROLLED BACK: the previous configuration is deployed again (hub $st; data kept). Nothing of the new configuration is running; fix the cause and run deploy.ps1 again."
        }
        throw "$Why`nThe rollback failed too: $rb`nThe hub's settings are saved in the resource group's tags (longarm-hub-prev-*): ./deploy.ps1 -Name $Name -ResourceGroup $rg (without -Recreate) deploys them again."
    }

    $imageText = if ($Image) { ", $Image" } else { '' }
    $headscaleText = if ($headscaleOn) { ', headscale' } else { '' }
    Write-Host "deploying $Name to $rg ($Location, $Size, panel $(([bool]$panelOn).ToString().ToLower()), storage auth $StorageAuth$headscaleText$imageText); this takes a few minutes"
    $query = '[properties.outputs.hubAddress.value, properties.outputs.fqdn.value, properties.outputs.storageAccount.value, properties.outputs.relayUdpPort.value, properties.outputs.panelUrl.value]'
    # one tab-separated line; panelUrl last (empty without the panel)
    $start = Get-Date
    $deadline = $start.AddSeconds($TimeoutSeconds)
    $logsHint = "az container logs -g $rg -n $Name --container-name hub"
    $deployment = 'longarm-hub-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddHHmmss')
    $err = Invoke-DeployRun $paramsFile $deployment
    if ($err) { Invoke-FailDeploy $err }
    $outLine = (Invoke-Az deployment group show -g $rg -n $deployment --query $query -o tsv) -join ''
    $outs = Split-Tsv $outLine 5
    $hubAddr, $fqdn, $account, $relayPort, $panelUrl = $outs[0], $outs[1], $outs[2], $outs[3], $outs[4]
    if (-not $hubAddr -or -not $fqdn -or -not $account) { throw "deployment returned no outputs: $outLine" }
    if (-not $relayPort) { $relayPort = '7443' }
    # the relay's TLS fallback is on the hub's TCP port (ACI: UDP 7445, TCP 7443)
    $hubPort = $hubAddr.Substring($hubAddr.LastIndexOf(':') + 1)
    $relayTcp = ''
    if ($relayPort -ne $hubPort) { $relayTcp = "tcp=${fqdn}:$hubPort&" }
    if ($StorageAuth -eq 'key') {
        $key = (Invoke-Az storage account keys list -g $rg -n $account --query '[0].value' -o tsv) -join ''
    }

    $hubImageQuery = "containers[?name=='hub'].image"
    $newImage = (Invoke-Az container show -g $rg -n $Name --query $hubImageQuery -o tsv) -join ''
    if (-not $Recreate -and $curImage -and $curImage -eq $newImage -and $newImage -match ':(latest|edge)$') {
        Write-Host "pulling $newImage again (az container restart)"
        Invoke-Az container restart -g $rg -n $Name -o none | Out-Null
    }

    Write-Host "waiting for the hub at $hubAddr"
    $start = Get-Date
    while ($true) {
        $state = (Invoke-AzQuiet container show -g $rg -n $Name --query "containers[?name=='hub'].instanceView.currentState.state" -o tsv) -join ''
        if ($state -eq 'Running') { break }
        $stuck = Test-Stuck
        if ($stuck) { Invoke-FailDeploy $stuck }
        if ((Get-Date) -gt $deadline) { Invoke-FailDeploy "the hub container is not running ($state): $logsHint" }
        Start-Sleep -Seconds $script:Poll
    }
    # the bootstrap values of this start (a new boot_id; restart once if the
    # update didn't restart the hub)
    $restarted = $false
    while ($true) {
        $boot = Read-Bootstrap $account $key $bootFile
        if ($boot -and (Get-Value $boot 'boot_id') -ne $prevBoot) { break }
        if ((Get-Date) -gt $deadline) { Invoke-FailDeploy "no bootstrap values from this start: $logsHint" }
        if ($prevBoot -and -not $restarted -and ((Get-Date) - $start).TotalSeconds -gt 240) {
            Write-Host 'the hub did not restart: az container restart'
            Invoke-Az container restart -g $rg -n $Name -o none | Out-Null
            $restarted = $true
        }
        Start-Sleep -Seconds 5
    }
    $port = [int]$hubPort
    while (-not (Test-TcpPort $fqdn $port)) {
        if ((Get-Date) -gt $deadline) { Invoke-FailDeploy "$hubAddr does not answer: $logsHint" }
        Start-Sleep -Seconds 5
    }
    # the new configuration runs: the settings snapshot has done its job
    if ($script:Deleted -or $prevSrc -eq 'snapshot') { Clear-Snapshot }
    $fp = Get-Value $boot 'ca_fingerprint'
    $code = Get-Value $boot 'admin_join_code'
    $expires = Get-Value $boot 'admin_join_code_expires'
    $adminJoined = Get-Value $boot 'admin_joined'

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
        $panelContainer = if ($StorageAuth -eq 'identity') { 'hub' } else { 'caddy' }
        if (-not $ok) { Write-Warning "$panelUrl/healthz does not answer yet (Let's Encrypt can take a few minutes): az container logs -g $rg -n $Name --container-name $panelContainer" }
    }

    # the tailnet: the hub's view of its Headscale sidecar (/healthz "headscale":
    # ok | starting | down; the detail: longarm admin tailnet status)
    $tailnetText = 'none (-Headscale for a self-hosted one, or -TailscaleSecretFile)'
    if ($headscaleOn) {
        $hs = ''
        for ($i = 0; $i -lt 18 -and $hs -ne 'ok'; $i++) {
            try {
                $health = Invoke-RestMethod -UseBasicParsing -TimeoutSec 10 -Uri "$panelUrl/healthz"
                if ($health.PSObject.Properties.Name -contains 'headscale') { $hs = [string]$health.headscale }
            } catch {
                $hs = ''
            }
            if ($hs -ne 'ok') { Start-Sleep -Seconds $script:Poll }
        }
        $hsText = if ($hs) { $hs } else { 'unknown' }
        $tailnetText = "headscale ${panelUrl}: $hsText"
        if ($hs -ne 'ok') { Write-Warning "Headscale is $hsText after 3 min: az container logs -g $rg -n $Name --container-name headscale (and --container-name hub); longarm admin tailnet status" }
    } elseif ($TailscaleSecretFile) {
        $tailnetText = 'tailscale (OAuth client)'
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# Rough monthly cost (Linux ACI pay-as-you-go, about 0.0405 USD per vCPU-hour
# and 0.0045 USD per GB-hour in US regions; other regions differ by up to ~20 %).
$cpu = 1.0; $mem = 1.5
if ($Size -eq 'medium') { $cpu = 2.0; $mem = 3.0 }
# Caddy (storage-auth identity: the hub serves the panel itself)
if ($panelOn -and $StorageAuth -eq 'key') { $cpu += 0.25; $mem += 0.3 }
# the Headscale sidecar
if ($headscaleOn) { $cpu += 0.25; $mem += 0.3 }
$cost = [math]::Round(730 * ($cpu * 0.0405 + $mem * 0.0045) + 1)

$panelText = if ($panelUrl) { $panelUrl } else { 'off (-NoPanel)' }
Write-Host @"

longarm-hub is up.
  hub address     $hubAddr
  CA fingerprint  $fp
  panel           $panelText
  tailnet         $tailnetText
  resource group  $rg (storage account $account, storage auth $StorageAuth)
  cost            about $cost USD/month (compute; plus egress over 100 GB/month, relay traffic counts)

"@
if ($code) {
    Write-Host @"
1. Your laptop (admin): install longarm (https://github.com/lubobill1990/longarm/releases), then
     longarm join $hubAddr $code --ca-fingerprint $fp
   The code is one-time and valid until $expires (a new one: ./deploy.ps1 -Name $Name -AdminCode).
"@
} elseif ($adminJoined -eq 'false') {
    # storage-auth identity: the code is only in the container's bootstrap.env
    Write-Host @"
1. Your laptop (admin): install longarm (https://github.com/lubobill1990/longarm/releases), then
     ./deploy.ps1 -Name $Name -ResourceGroup $rg -AdminCode      # a one-time admin join code (24 h)
     longarm join $hubAddr <code> --ca-fingerprint $fp
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
