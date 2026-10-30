# relay.ps1: deploy.ps1 -Relay / -RelayRemove (dot-sourced by deploy.ps1,
# which has checked the parameters and defined Invoke-Az, Invoke-AzQuiet,
# Split-Tsv, Get-RegistryHost, Register-Provider, Test-TcpPort and $rg).
# The twin of relay.sh; README "External relay".
#
# The hub's external longnet relay: its own container group <name>-relay
# (relay.json, compiled from relay.bicep) with its own public IP and DNS
# label, so a hub redeploy never restarts it. Its certificate comes from the
# hub's CA (az container exec ... longarm-hub-relay-cert) and goes to the
# container as a secure environment variable; the relay registers with the
# hub, which advertises it to the nodes. No storage and no state of its own.

$relayTemplate = Join-Path $PSScriptRoot 'relay.json'
$relayGroup = "$Name-relay"
if (-not (Test-Path $relayTemplate)) { throw "$relayTemplate is missing (run from a checkout of the repository)" }
if (-not (Test-ResourceGroup)) { throw "resource group $rg does not exist: deploy the hub first (./deploy.ps1 -Name $Name -Location <region>)" }

if ($RelayRemove) {
    if (-not $Yes) {
        Write-Host "This deletes the external relay $relayGroup in $rg (the hub keeps its embedded relay)."
        $answer = Read-Host "Type the hub name ($Name) to continue"
        if ($answer -ne $Name) { throw 'aborted' }
    }
    Invoke-Az container delete -g $rg -n $relayGroup --yes -o none | Out-Null
    Write-Host "deleted $relayGroup. The hub drops it from the relay set 7 days after it last saw it; at once with: longarm admin revoke $relayGroup"
    return
}

if (-not $Location) { $Location = (Invoke-Az group show -n $rg --query location -o tsv) -join '' }
Register-Provider 'Microsoft.ContainerInstance'

# The hub: its image (the relay runs the same one), its registry login, its
# STUN servers, and that it runs (it issues the certificate and is what the
# relay registers with).
$q = "[containers[?name=='hub'].image | [0], ipAddress.fqdn, imageRegistryCredentials[0].server, imageRegistryCredentials[0].identity, containers[?name=='hub'].instanceView.currentState.state | [0], containers[?name=='hub'].environmentVariables[] | [?name=='LONGARM_HUB_RELAY_STUN_ADVERTISE'].value | [0]]"
$hubLine = (Invoke-AzQuiet container show -g $rg -n $Name -o tsv --query $q) -join "`t"
$hubF = Split-Tsv $hubLine 6
$hubImage, $hubFqdn, $hubReg, $hubRegIdentity, $hubState, $hubStun = $hubF[0], $hubF[1], $hubF[2], $hubF[3], $hubF[4], $hubF[5]
if (-not $hubImage -or -not $hubFqdn) { throw "no hub container group $Name in ${rg}: deploy the hub first" }
if ($hubState -ne 'Running') {
    $st = if ($hubState) { $hubState } else { 'not running' }
    throw "the hub is ${st}: the relay gets its certificate from the hub and registers with it - start the hub first (az container start -g $rg -n $Name)"
}
$relayImage = if ($Image) { $Image } else { $hubImage }
$relayRegHost = Get-RegistryHost $relayImage
$relayPassword = $env:LONGARM_REGISTRY_PASSWORD
$relayUser = $RegistryUser

# the registry: the hub's managed identity (an ACR in identity mode), a login, or anonymous
$relayAcr = ''
if ($hubRegIdentity -and $relayRegHost -eq $hubReg) {
    $relayAcr = $hubReg.Split('.')[0]
} elseif (-not $relayUser -and $relayRegHost -like '*.azurecr.io') {
    $regAcr = (Invoke-AzQuiet acr list -g $rg --query "[?loginServer=='$relayRegHost'].name | [0]" -o tsv) -join ''
    if ($regAcr) {
        $creds = Split-Tsv ((Invoke-Az acr credential show -n $regAcr -g $rg --query '[username, passwords[0].value]' -o tsv) -join "`t") 2
        $relayUser, $relayPassword = $creds[0], $creds[1]
        if (-not $relayUser -or -not $relayPassword) { throw "no admin credentials for the registry $regAcr (az acr update -n $regAcr --admin-enabled true)" }
    }
}
if (-not $relayAcr -and -not $relayUser -and $hubReg -and $relayRegHost -eq $hubReg) {
    throw "the hub pulls its image with a login for $hubReg, which can't be read back: pass -RegistryUser (password in `$env:LONGARM_REGISTRY_PASSWORD)"
}
if ($relayUser -and -not $relayPassword) { throw '-RegistryUser needs $env:LONGARM_REGISTRY_PASSWORD' }

# STUN: the hub's (the template sets self[,<servers>]) unless given
$relayStun = $StunServers
if (-not $relayStun) {
    if ($hubStun -ceq 'self') { $relayStun = 'none' } elseif ($hubStun -like 'self,*') { $relayStun = $hubStun.Substring(5) }
}

# the previous relay (rollback): its image
$prevRelayImage = ((Invoke-AzQuiet container show -g $rg -n $relayGroup --query 'containers[0].image' -o tsv) -join '').Trim()
if ($LASTEXITCODE -ne 0 -or $prevRelayImage -ceq 'None') { $prevRelayImage = '' }

# The certificate, issued by the hub from its CA: base64 of a tar.gz with
# ca.crt, node.crt, node.key (role relay, name <name>-relay, valid a year;
# the relay renews it through the hub). A private key: only in memory and
# the deployment's secure parameter.
Write-Host "issuing the relay certificate from the hub's CA (az container exec $Name longarm-hub-relay-cert)"
$certOut = Invoke-AzQuiet container exec -g $rg -n $Name --container-name hub --exec-command /usr/local/bin/longarm-hub-relay-cert
$relayTls = ''
foreach ($l in (($certOut -join "`n") -split "`r?`n")) {
    if ($l -cmatch '^[A-Za-z0-9+/=]{200,}$') { $relayTls = $l }
}
if (-not $relayTls) {
    throw "no relay certificate from the hub (az container exec ... /usr/local/bin/longarm-hub-relay-cert): its image may predate external relays - upgrade the hub first (./deploy.ps1 -Name $Name -Image <image:tag>)"
}
try {
    $gz = [System.IO.Compression.GZipStream]::new([System.IO.MemoryStream]::new([Convert]::FromBase64String($relayTls)), [System.IO.Compression.CompressionMode]::Decompress)
    $ms = [System.IO.MemoryStream]::new()
    $gz.CopyTo($ms)
    $tar = $ms.ToArray()
} catch {
    throw "the hub's relay certificate bundle is not a base64 tar.gz: $_"
}
# the tar's member names (512-byte headers: the name in the first 100 bytes)
$names = @()
for ($off = 0; $off + 512 -le $tar.Length; ) {
    $n = [System.Text.Encoding]::ASCII.GetString($tar, $off, 100).TrimEnd([char]0)
    if (-not $n) { break }
    $entrySize = [Convert]::ToInt64(([System.Text.Encoding]::ASCII.GetString($tar, $off + 124, 12).Trim([char]0, ' ')), 8)
    $names += $n
    $off += 512 + [int]([math]::Ceiling($entrySize / 512) * 512)
}
if ((($names | Sort-Object) -join ' ') -ne 'ca.crt node.crt node.key') { throw "the hub's relay certificate bundle is not ca.crt, node.crt, node.key (got: $($names -join ' '))" }

$relayTmp = Join-Path ([System.IO.Path]::GetTempPath()) ("longarm-relay-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $relayTmp | Out-Null
if ($PSVersionTable.PSEdition -eq 'Core' -and -not $IsWindows) { & chmod 700 $relayTmp }
try {
    function Write-RelayParamFile([string]$File, [string]$Img) {
        $p = [ordered]@{
            name       = @{ value = $Name }
            image      = @{ value = $Img }
            hubAddress = @{ value = "${hubFqdn}:7443" }
            relayTls   = @{ value = $relayTls }
        }
        if ($relayStun) {
            $s = $relayStun
            if ($s -eq 'none') { $s = '' }
            $p.stunServers = @{ value = $s }
        }
        if ($relayAcr) { $p.acrName = @{ value = $relayAcr } }
        elseif ($relayUser) {
            if ($relayRegHost) { $p.registryServer = @{ value = $relayRegHost } }
            $p.registryUsername = @{ value = $relayUser }
            $p.registryPassword = @{ value = $relayPassword }
        }
        $doc = @{ contentVersion = '1.0.0.0'; parameters = $p }
        [System.IO.File]::WriteAllText($File, ($doc | ConvertTo-Json -Depth 5))
    }
    $relayParams = Join-Path $relayTmp 'relay.json'
    $relayRollback = Join-Path $relayTmp 'relay-rollback.json'
    Write-RelayParamFile $relayParams $relayImage
    Write-Host 'validating the relay deployment (nothing is changed yet)'
    Invoke-AzQuiet deployment group validate -g $rg --template-file $relayTemplate --parameters "@$relayParams" -o none | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'the relay deployment does not validate (e.g. a policy violation; az deployment group validate shows why): nothing was changed' }
    if ($prevRelayImage) {
        Write-RelayParamFile $relayRollback $prevRelayImage
        Invoke-AzQuiet deployment group validate -g $rg --template-file $relayTemplate --parameters "@$relayRollback" -o none | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'the previous relay configuration (the rollback) does not validate: nothing was changed' }
    }

    function Invoke-RelayRun([string]$File, [string]$Dep) {
        # deploy and wait; the error text, or $null
        Invoke-AzQuiet deployment group create -g $rg -n $Dep --template-file $relayTemplate --parameters "@$File" --no-wait -o none | Out-Null
        if ($LASTEXITCODE -ne 0) { return "az deployment group create $Dep failed" }
        $until = (Get-Date).AddSeconds($TimeoutSeconds)
        $dstate = ''
        while ($dstate -ne 'Succeeded') {
            Start-Sleep -Seconds $script:Poll
            $dstate = ((Invoke-AzQuiet deployment group show -g $rg -n $Dep --query properties.provisioningState -o tsv) -join '').Trim()
            if ($dstate -eq 'Failed' -or $dstate -eq 'Canceled') {
                Invoke-AzQuiet deployment operation group list -g $rg -n $Dep --query "[?properties.provisioningState=='Failed'].{resource: properties.targetResource.resourceName, error: properties.statusMessage.error}" -o json | Write-Host
                return "deployment ${Dep}: $dstate (errors above)"
            }
            if ((Get-Date) -gt $until) { return "deployment $Dep is still $dstate after ${TimeoutSeconds}s" }
        }
        $null
    }
    $script:RelayFqdn = ''
    function Test-RelayUp([string]$Dep) {
        # the relay runs, registered with the hub (its log line), and its TLS fallback answers: the error text, or $null
        $until = (Get-Date).AddSeconds($TimeoutSeconds)
        $st = ''
        while ($st -ne 'Running') {
            $st = ((Invoke-AzQuiet container show -g $rg -n $relayGroup --query 'containers[0].instanceView.currentState.state' -o tsv) -join '').Trim()
            if ($st -eq 'Running') { break }
            if ((Get-Date) -gt $until) { return "the relay container is $st" }
            Start-Sleep -Seconds $script:Poll
        }
        $o = Split-Tsv ((Invoke-AzQuiet deployment group show -g $rg -n $Dep -o tsv --query '[properties.outputs.fqdn.value, properties.outputs.relayTcp.value]') -join "`t") 2
        $script:RelayFqdn, $relayTcp = $o[0], $o[1]
        if (-not $script:RelayFqdn -or $relayTcp -notlike '*:*') { return "deployment $Dep returned no outputs" }
        $tcpHost, $tcpPort = $relayTcp.Substring(0, $relayTcp.LastIndexOf(':')), [int]$relayTcp.Substring($relayTcp.LastIndexOf(':') + 1)
        while (-not ((Invoke-AzQuiet container logs -g $rg -n $relayGroup --container-name relay) -join "`n").Contains('relay: subscribed to the hub')) {
            if ((Get-Date) -gt $until) { return "the relay did not register with the hub (az container logs -g $rg -n $relayGroup)" }
            Start-Sleep -Seconds $script:Poll
        }
        while (-not (Test-TcpPort $tcpHost $tcpPort)) {
            if ((Get-Date) -gt $until) { return "$relayTcp does not answer" }
            Start-Sleep -Seconds 5
        }
        $null
    }
    function Invoke-RelayFailure([string]$Why) {
        if (-not $prevRelayImage) {
            throw "$Why`nThe hub is unaffected (its embedded relay keeps working). Fix the cause and run ./deploy.ps1 -Name $Name -Relay again, or remove the relay: ./deploy.ps1 -Name $Name -RelayRemove"
        }
        Write-Warning $Why
        Write-Warning "rolling back: deploying the previous relay again (image $prevRelayImage)"
        $dep = 'longarm-relay-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddHHmmss') + '-rollback'
        $e = Invoke-RelayRun $relayRollback $dep
        if (-not $e) { $e = Test-RelayUp $dep }
        if (-not $e) { throw "$Why`nROLLED BACK: the previous relay ($prevRelayImage) runs again. The hub is unaffected." }
        throw "$Why`nThe rollback failed too: $e`nThe hub is unaffected (its embedded relay keeps working)."
    }

    Write-Host "deploying the external relay $relayGroup to $rg ($Location, $relayImage); a few minutes"
    $relayDep = 'longarm-relay-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddHHmmss')
    $err = Invoke-RelayRun $relayParams $relayDep
    if ($err) { Invoke-RelayFailure $err }
    $err = Test-RelayUp $relayDep
    if ($err) { Invoke-RelayFailure $err }
} finally {
    Remove-Item -Recurse -Force $relayTmp -ErrorAction SilentlyContinue
}

Write-Host @"

The external relay is up.
  relay           $($script:RelayFqdn)  udp 7445 (QUIC + STUN), 7446 (STUN), tcp 7443 (TLS fallback)
  registered with ${hubFqdn}:7443 (longarm admin relays shows it and how many workers use it)
  cost            about 16 USD/month (0.5 vCPU / 0.5 GB; plus egress over 100 GB/month: relayed traffic)

Workers that know the relay set (this release and later) listen on it within
seconds; clients reach them through it from their next command. Older
workers keep using the hub's embedded relay until they update (longarm
worker update, the tray's update banner, or self-update). Nothing needs
signing in again. Hub redeploys no longer drop connections that go through
this relay.

Upgrade the relay with the hub's image: ./deploy.ps1 -Name $Name -Relay
Remove it:                              ./deploy.ps1 -Name $Name -RelayRemove
"@
