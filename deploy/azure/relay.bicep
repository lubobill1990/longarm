// The hub's external longnet relay on Azure Container Instances
// (deploy/azure/README.md "External relay"; deploy.sh --relay).
//
//   az deployment group create -g <rg> --template-file relay.bicep -p name=myhub relayTls=<base64 bundle>
//
// Its own container group <name>-relay with its own public IP and DNS label
// (<name>-relay.<region>.azurecontainer.io), so a hub redeploy or restart
// never restarts it: relayed client <-> worker connections keep going while
// the hub is away. It runs longarm-relay from the hub image, registered with
// the hub (--hub): the hub advertises it to the nodes as part of its relay
// set and sends it the revocation list and port-link pairs, which it keeps
// (on the container disk) and enforces on its own. No storage, no state to
// back up: the certificate comes in relayTls (issued from the hub's CA by
// deploy.sh: longarm-hub-relay-cert) and the policy from the hub.
// relay.json is this file compiled (bicep build); CI checks that it is up to
// date.

@description('The hub\'s name (deploy.sh --name): the relay is <name>-relay.<region>.azurecontainer.io.')
@minLength(3)
@maxLength(40)
param name string

@description('Azure region (default: the resource group\'s).')
param location string = resourceGroup().location

@description('Image with longarm-relay: the hub\'s image (deploy.sh uses the running hub\'s).')
param image string = 'ghcr.io/lubobill1990/longarm-hub:latest'

@description('The hub the relay registers with (host:port).')
param hubAddress string = '${name}.${location}.azurecontainer.io:7443'

@description('The relay certificate bundle: base64 of a tar.gz with ca.crt, node.crt and node.key (role relay, from the hub\'s CA: az container exec ... longarm-hub-relay-cert).')
@secure()
param relayTls string

@description('UDP port of the relay (QUIC + STUN); STUN\'s second port is this + 1. Not 7443: ACI refuses one port number for TCP and UDP, and the TLS fallback (nodes whose UDP is blocked) is on 7443/tcp, as on the hub.')
@minValue(7444)
@maxValue(65534)
param relayUdpPort int = 7445

@description('External STUN servers the relay advertises next to its own ports (comma-separated host:port; empty = only its own). ACI rewrites the source address of inbound UDP, so its own answers are useless there (README "Direct connections").')
param stunServers string = 'stun.cloudflare.com:3478,stun.l.google.com:19302'

@description('vCPUs of the relay container (QUIC crypto for relayed sessions; 0.5 carries a fleet of tens of nodes).')
@allowed([
  '0.5'
  '1'
  '2'
])
param cpu string = '0.5'

@description('Memory of the relay container in GB.')
@allowed([
  '0.5'
  '1'
  '2'
])
param memoryInGB string = '0.5'

@description('storageAuth identity hubs: pull the image from this Azure Container Registry (in this resource group) with the relay\'s own managed identity <name>-relay-id (AcrPull on the registry only; never the hub\'s identity, which can read the replicated database and with it the CA). Empty: registryUsername or anonymous.')
param acrName string = ''

@description('Registry login for a private image (empty = anonymous pull).')
param registryServer string = 'ghcr.io'
param registryUsername string = ''
@secure()
param registryPassword string = ''

var relayName = '${name}-relay'
var relayTcpPort = 7443
var fqdn = '${relayName}.${location}.azurecontainer.io'
var useAcr = !empty(acrName)
var tags = {
  'longarm-hub': name
  'longarm-role': 'relay'
}
var stunAdvertise = empty(stunServers) ? 'self' : 'self,${stunServers}'

// The relay's own identity: AcrPull on the registry and nothing else. The
// hub's identity (<name>-id) may read the database replica, which holds the
// CA key in identity mode: a relay container must never carry it.
resource identity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = if (useAcr) {
  name: '${relayName}-id'
  location: location
  tags: tags
}

resource acr 'Microsoft.ContainerRegistry/registries@2023-07-01' existing = if (useAcr) {
  name: acrName
}

resource acrRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (useAcr) {
  name: guid(resourceId('Microsoft.ContainerRegistry/registries', acrName), '${relayName}-id', '7f951dda-4ed3-4680-a7ca-43fe172d538d')
  scope: acr
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '7f951dda-4ed3-4680-a7ca-43fe172d538d')
    principalId: identity!.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

var relayContainer = {
  name: 'relay'
  properties: {
    image: image
    command: [
      '/usr/local/bin/longarm-relay'
      '--udp'
      ':${relayUdpPort}'
      '--tcp'
      ':${relayTcpPort}'
      '--stun2'
      ':${relayUdpPort + 1}'
      '--tls-dir'
      '/data/relay/tls'
      '--state-dir'
      '/data/relay/state'
      '--metrics'
      '127.0.0.1:7801'
      '--hub'
      hubAddress
      '--advertise'
      fqdn
      '--stun-advertise'
      stunAdvertise
    ]
    environmentVariables: [
      { name: 'LONGARM_RELAY_TLS_B64', secureValue: relayTls }
    ]
    resources: {
      requests: {
        cpu: json(cpu)
        memoryInGB: json(memoryInGB)
      }
    }
    ports: [
      { port: relayTcpPort, protocol: 'TCP' }
      { port: relayUdpPort, protocol: 'UDP' }
      { port: relayUdpPort + 1, protocol: 'UDP' }
    ]
    // alive = the process answers; a relay waiting for its hub's first
    // policy is alive (longarm-relay healthcheck without --ready)
    livenessProbe: {
      exec: {
        command: [
          '/usr/local/bin/longarm-relay'
          'healthcheck'
          '--addr'
          '127.0.0.1:7801'
        ]
      }
      initialDelaySeconds: 20
      periodSeconds: 30
      failureThreshold: 3
      timeoutSeconds: 10
    }
  }
}

resource group 'Microsoft.ContainerInstance/containerGroups@2025-09-01' = {
  name: relayName
  location: location
  tags: tags
  identity: useAcr ? {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identity.id}': {}
    }
  } : null
  dependsOn: [
    acrRole // the pull right before the first pull
  ]
  properties: {
    osType: 'Linux'
    sku: 'Standard'
    restartPolicy: 'Always'
    containers: [relayContainer]
    imageRegistryCredentials: useAcr ? [
      {
        server: acr!.properties.loginServer
        identity: identity.id
      }
    ] : empty(registryUsername) ? [] : [
      {
        server: registryServer
        username: registryUsername
        password: registryPassword
      }
    ]
    ipAddress: {
      type: 'Public'
      dnsNameLabel: relayName
      ports: [
        { port: relayTcpPort, protocol: 'TCP' }
        { port: relayUdpPort, protocol: 'UDP' }
        { port: relayUdpPort + 1, protocol: 'UDP' }
      ]
    }
  }
}

output fqdn string = group.properties.ipAddress.fqdn
output ipAddress string = group.properties.ipAddress.ip
output containerGroup string = group.name
@description('The relay as nodes reach it: UDP (QUIC) and the TLS fallback.')
output relayUdp string = '${fqdn}:${relayUdpPort}'
output relayTcp string = '${fqdn}:${relayTcpPort}'
