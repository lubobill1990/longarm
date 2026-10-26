// longarm-hub on Azure Container Instances (deploy/azure/README.md).
//
//   az deployment group create -g <rg> --template-file main.bicep -p name=myhub
//
// One container group with a public IP and DNS label
// (<name>.<region>.azurecontainer.io): the hub on 7443/tcp (gRPC, mTLS end to
// end, relay TLS fallback), 7445/udp (longnet relay, QUIC) and 7446/udp (second
// STUN port), and
// optionally Caddy on 80/443 (Let's Encrypt) in front of the panel on
// 127.0.0.1:7480. The SQLite database stays on the container's local disk and
// Litestream streams it to a blob container (restored on start); CA, hub.toml,
// server certificates, release store and bootstrap.env live on an Azure Files
// share. azuredeploy.json is this file compiled (bicep build) for the
// "Deploy to Azure" button; CI checks that it is up to date.

@description('Hub name: the DNS label (<name>.<region>.azurecontainer.io) and the prefix of the resources. Lowercase letters, digits and hyphens, starting with a letter.')
@minLength(3)
@maxLength(40)
param name string

@description('Azure region (default: the resource group\'s).')
param location string = resourceGroup().location

@description('longarm-hub image. Pin a version (ghcr.io/<owner>/longarm-hub:X.Y.Z) for reproducible upgrades; :latest is the newest release, :edge the main branch.')
param image string = 'ghcr.io/lubobill1990/longarm-hub:latest'

@description('Hub size: small = 1 vCPU / 1.5 GB, medium = 2 vCPU / 3 GB.')
@allowed([
  'small'
  'medium'
])
param size string = 'small'

@description('Serve the web panel and install scripts over HTTPS on 80/443 with a Let\'s Encrypt certificate for the azurecontainer.io name: a Caddy sidecar (storageAuth key), or the hub itself with the certificate in its replicated database (storageAuth identity).')
param enablePanel bool = true

@description('E-mail for Let\'s Encrypt expiry notices (optional).')
param acmeEmail string = ''

@description('Caddy image of the panel sidecar (pinned by digest; point it at a mirror if Docker Hub rate limits bite).')
param caddyImage string = 'docker.io/library/caddy:2.11.7-alpine@sha256:eb4d35b7452d1ab6171fbdbe8d9275b2301dad6dad92115d91e19fb9b24d510f'

@description('Send container logs to a new Log Analytics workspace (about 2-3 USD per GB ingested).')
param enableLogAnalytics bool = false

@description('Telemetry (docs/telemetry.md): a Log Analytics workspace and a workspace-based Application Insights resource the hub sends the fleet\'s scrubbed telemetry to. With storageAuth identity the hub authenticates with its managed identity (Monitoring Metrics Publisher) and the resource accepts no instrumentation-key ingestion.')
param enableTelemetry bool = false

@description('Days Application Insights / Log Analytics keep telemetry.')
@minValue(30)
@maxValue(730)
param telemetryRetentionDays int = 90

@description('Diagnostic bundles (longarm diag --upload): a blob container "diagnostics" the hub writes with its managed identity (storageAuth identity only), deleted by a lifecycle rule after diagnosticsRetentionDays.')
param enableDiagnostics bool = false

@minValue(1)
@maxValue(365)
param diagnosticsRetentionDays int = 30

@description('Storage redundancy of the account holding the Litestream replica and the state share.')
@allowed([
  'Standard_LRS'
  'Standard_ZRS'
  'Standard_GRS'
])
param storageSku string = 'Standard_LRS'

@description('Lifetime in hours of the one-time admin join code created on the first start.')
@minValue(1)
@maxValue(168)
param adminCodeTtlHours int = 24

@description('UDP port of the longnet relay (QUIC + STUN); the second STUN port is this + 1. Not 7443: ACI refuses one port number for TCP and UDP (DuplicateContainerPorts / DuplicateIpAddressPorts). Workers listen on longnet://<fqdn>:<port>?tcp=<fqdn>:7443 (the relay TLS fallback uses the hub TCP port).')
@minValue(7444)
@maxValue(65534)
param relayUdpPort int = 7445

@description('External STUN servers the hub advertises to the nodes next to its own STUN ports (comma-separated host:port; empty = only its own). ACI rewrites the source address of inbound UDP, so the hub\'s own STUN answers are useless there: direct (peer-to-peer) connections need these (README "Direct connections").')
param stunServers string = 'stun.cloudflare.com:3478,stun.l.google.com:19302'

@description('Expiry of the Litestream SAS (blob container scoped); every deployment (deploy.sh re-run) renews it.')
param sasExpiry string = dateTimeAdd(utcNow('u'), 'P10Y', 'yyyy-MM-ddTHH:mm:ssZ')

@description('How the hub reaches its storage account. key: the account key (Azure Files share for the data dir, a SAS for Litestream). identity: a user-assigned managed identity with Storage Blob Data Contributor on the replica container and shared key access off, for subscriptions whose policy denies it: no file shares, the data dir is the container\'s own disk and the CA is kept in the replicated database (LONGARM_HUB_CA_IN_DB), and the hub serves the panel over https itself (no Caddy; its certificate in the database too). Needs Owner or User Access Administrator on the resource group (role assignments).')
@allowed([
  'key'
  'identity'
])
param storageAuth string = 'key'

@description('storageAuth identity: pull the image from this Azure Container Registry (in this resource group) with the managed identity (AcrPull). Empty: registryUsername or anonymous.')
param acrName string = ''

@description('Registry login for a private image (empty = anonymous pull).')
param registryServer string = 'ghcr.io'
param registryUsername string = ''
@secure()
param registryPassword string = ''

@description('Optional Tailscale OAuth client secret for the hub\'s [tailnet] provider (deploy/hub/README.md "Tailnet provider").')
@secure()
param tailscaleOAuthSecret string = ''

var sizes = {
  small: {
    cpu: 1
    memoryInGB: json('1.5')
  }
  medium: {
    cpu: 2
    memoryInGB: json('3.0')
  }
}
// the region's programmatic name ("East Asia" -> eastasia)
var fqdn = '${name}.${toLower(replace(location, ' ', ''))}.azurecontainer.io'
var hubAddress = '${fqdn}:7443'
var storageName = take('lahub${uniqueString(resourceGroup().id, name)}', 24)
var useKey = storageAuth == 'key'
var useAcr = !useKey && !empty(acrName)
// identity: the hub serves the panel over https itself (LONGARM_HUB_HTTPS):
// its certificate lives in the database, Caddy's would die with the container
var hubTls = !useKey && enablePanel
var useDiag = enableDiagnostics && !useKey
var tags = {
  'longarm-hub': name
}
var caddyfile = '''
{
	admin off
	{EMAIL}
}
{FQDN} {
	reverse_proxy 127.0.0.1:7480 {
		flush_interval -1
	}
}
'''
var caddyfileText = replace(replace(caddyfile, '{FQDN}', fqdn), '{EMAIL}', empty(acmeEmail) ? '' : 'email ${acmeEmail}')

resource storage 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: storageName
  location: location
  tags: tags
  sku: {
    name: storageSku
  }
  kind: 'StorageV2'
  properties: {
    accessTier: 'Hot'
    allowBlobPublicAccess: false
    // key: ACI mounts Azure Files with the account key, Litestream uses a
    // SAS signed with it. identity: Entra ID only
    allowSharedKeyAccess: useKey
    defaultToOAuthAuthentication: !useKey
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
  }
}

resource blobService 'Microsoft.Storage/storageAccounts/blobServices@2023-05-01' = {
  parent: storage
  name: 'default'
  properties: {
    deleteRetentionPolicy: {
      enabled: true
      days: 7
    }
    containerDeleteRetentionPolicy: {
      enabled: true
      days: 7
    }
  }
}

resource replicaContainer 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = {
  parent: blobService
  name: 'litestream'
  properties: {
    publicAccess: 'None'
  }
}

resource diagContainer 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = if (useDiag) {
  parent: blobService
  name: 'diagnostics'
  properties: {
    publicAccess: 'None'
  }
}

// bundles carry (scrubbed) logs: they go away on their own
resource lifecycle 'Microsoft.Storage/storageAccounts/managementPolicies@2023-05-01' = if (useDiag) {
  parent: storage
  name: 'default'
  properties: {
    policy: {
      rules: [
        {
          name: 'diagnostics-retention'
          enabled: true
          type: 'Lifecycle'
          definition: {
            filters: {
              blobTypes: ['blockBlob']
              prefixMatch: ['diagnostics/']
            }
            actions: {
              baseBlob: {
                delete: {
                  daysAfterModificationGreaterThan: diagnosticsRetentionDays
                }
              }
            }
          }
        }
      ]
    }
  }
}

resource fileService 'Microsoft.Storage/storageAccounts/fileServices@2023-05-01' = if (useKey) {
  parent: storage
  name: 'default'
  properties: {
    shareDeleteRetentionPolicy: {
      enabled: true
      days: 7
    }
  }
}

resource stateShare 'Microsoft.Storage/storageAccounts/fileServices/shares@2023-05-01' = if (useKey) {
  parent: fileService
  name: 'state'
  properties: {
    shareQuota: 10
  }
}

resource caddyShare 'Microsoft.Storage/storageAccounts/fileServices/shares@2023-05-01' = if (useKey && enablePanel) {
  parent: fileService
  name: 'caddy'
  properties: {
    shareQuota: 1
  }
}

resource logs 'Microsoft.OperationalInsights/workspaces@2023-09-01' = if (enableLogAnalytics) {
  name: '${name}-logs'
  location: location
  tags: tags
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: 30
  }
}

// storageAuth identity: Litestream (DefaultAzureCredential, AZURE_CLIENT_ID)
// writes the replica as this identity; it pulls the image from acrName too
resource identity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = if (!useKey) {
  name: '${name}-id'
  location: location
  tags: tags
}

// Storage Blob Data Contributor, on the replica container only
resource replicaRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!useKey) {
  name: guid(replicaContainer.id, '${name}-id', 'ba92f5b4-2d11-453d-a403-e96b0029c9fe')
  scope: replicaContainer
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'ba92f5b4-2d11-453d-a403-e96b0029c9fe')
    principalId: identity!.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

// Storage Blob Data Contributor on the diagnostics container only
resource diagRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (useDiag) {
  name: guid(resourceId('Microsoft.Storage/storageAccounts/blobServices/containers', storage.name, 'default', 'diagnostics'), '${name}-id', 'ba92f5b4-2d11-453d-a403-e96b0029c9fe')
  scope: diagContainer
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'ba92f5b4-2d11-453d-a403-e96b0029c9fe')
    principalId: identity!.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

resource telemetryWorkspace 'Microsoft.OperationalInsights/workspaces@2023-09-01' = if (enableTelemetry) {
  name: '${name}-telemetry'
  location: location
  tags: tags
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: telemetryRetentionDays
  }
}

resource appInsights 'Microsoft.Insights/components@2020-02-02' = if (enableTelemetry) {
  name: '${name}-ai'
  location: location
  tags: tags
  kind: 'other'
  properties: {
    Application_Type: 'other'
    WorkspaceResourceId: telemetryWorkspace.id
    RetentionInDays: telemetryRetentionDays
    // identity: only Entra ID tokens (the hub's managed identity) may ingest
    DisableLocalAuth: !useKey
    IngestionMode: 'LogAnalytics'
  }
}

// Monitoring Metrics Publisher: ingest into this Application Insights only
resource telemetryRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (enableTelemetry && !useKey) {
  name: guid(resourceId('Microsoft.Insights/components', '${name}-ai'), '${name}-id', '3913510d-42f4-4e42-8a64-420c390055eb')
  scope: appInsights
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '3913510d-42f4-4e42-8a64-420c390055eb')
    principalId: identity!.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

resource acr 'Microsoft.ContainerRegistry/registries@2023-07-01' existing = if (useAcr) {
  name: acrName
}

// AcrPull
resource acrRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (useAcr) {
  name: guid(resourceId('Microsoft.ContainerRegistry/registries', acrName), '${name}-id', '7f951dda-4ed3-4680-a7ca-43fe172d538d')
  scope: acr
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '7f951dda-4ed3-4680-a7ca-43fe172d538d')
    principalId: identity!.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

var storageKey = useKey ? storage.listKeys().keys[0].value : ''
var replicaSas = useKey
  ? storage.listServiceSas('2023-05-01', {
      canonicalizedResource: '/blob/${storage.name}/litestream'
      signedResource: 'c'
      signedPermission: 'racwdl'
      signedProtocol: 'https'
      signedExpiry: sasExpiry
    }).serviceSasToken
  : ''
var probe = {
  exec: {
    command: [
      '/usr/local/bin/longarm-hub'
      'healthcheck'
    ]
  }
}

var hubEnv = concat(
  [
    { name: 'LONGARM_HUB_DATA_DIR', value: '/data' }
    { name: 'LONGARM_HUB_CONFIG', value: '/data/hub.toml' }
    { name: 'LONGARM_HUB_DB', value: '/var/lib/longarm-hub/hub.db' }
    { name: 'LONGARM_HUB_ADVERTISE', value: fqdn }
    { name: 'LONGARM_HUB_HTTP', value: '127.0.0.1:7480' }
    { name: 'LONGARM_HUB_BOOTSTRAP', value: '1' }
    { name: 'LONGARM_HUB_BOOTSTRAP_TTL', value: '${adminCodeTtlHours}h' }
    { name: 'LONGARM_HUB_RELAY_UDP', value: ':${relayUdpPort}' }
    { name: 'LONGARM_HUB_RELAY_STUN2', value: ':${relayUdpPort + 1}' }
    { name: 'LONGARM_HUB_LITESTREAM_CONFIG', value: '/etc/longarm-hub/litestream-abs.yml' }
    { name: 'LONGARM_HUB_BLOB_ACCOUNT', value: storage.name }
    { name: 'LONGARM_HUB_BLOB_CONTAINER', value: replicaContainer.name }
    // this cloud's blob endpoint (public, China, Government)
    { name: 'LONGARM_HUB_BLOB_ENDPOINT', value: storage.properties.primaryEndpoints.blob }
  ],
  // key: a SAS for the replica container only: the hub process (Litestream's
  // child) never sees the account key, which opens the share with the CA.
  // identity: the managed identity, and the CA in the database (the only
  // thing that outlives the container)
  useKey
    ? [{ name: 'LITESTREAM_AZURE_SAS_TOKEN', secureValue: replicaSas }]
    : [
        { name: 'AZURE_CLIENT_ID', value: identity!.properties.clientId }
        { name: 'LONGARM_HUB_CA_IN_DB', value: '1' }
      ],
  enablePanel ? [{ name: 'LONGARM_HUB_PUBLIC_URL', value: 'https://${fqdn}' }] : [],
  // the connection string names the resource; with identity it is useless
  // without the hub's token (local auth off)
  enableTelemetry ? [{ name: 'LONGARM_HUB_APPINSIGHTS_CONNECTION_STRING', secureValue: appInsights!.properties.ConnectionString }] : [],
  useDiag ? [{ name: 'LONGARM_HUB_DIAG_BLOB_URL', value: '${storage.properties.primaryEndpoints.blob}diagnostics' }] : [],
  hubTls
    ? concat(
        [
          { name: 'LONGARM_HUB_HTTPS', value: ':443' }
          { name: 'LONGARM_HUB_HTTPS_REDIRECT', value: ':80' }
        ],
        empty(acmeEmail) ? [] : [{ name: 'LONGARM_HUB_ACME_EMAIL', value: acmeEmail }]
      )
    : [],
  empty(tailscaleOAuthSecret) ? [] : [{ name: 'LONGARM_HUB_TAILSCALE_OAUTH_SECRET', secureValue: tailscaleOAuthSecret }],
  // self = the hub's own STUN ports; it drops them once it sees that ACI
  // source-NATs them (internal/relay snatVerdict). Always set: deploy.sh
  // reads it back on a re-run.
  [{ name: 'LONGARM_HUB_RELAY_STUN_ADVERTISE', value: empty(stunServers) ? 'self' : 'self,${stunServers}' }]
)

var hubPorts = [
  { port: 7443, protocol: 'TCP' }
  { port: relayUdpPort, protocol: 'UDP' }
  { port: relayUdpPort + 1, protocol: 'UDP' }
]

var hubContainer = {
  name: 'hub'
  properties: {
    image: image
    resources: {
      requests: sizes[size]
    }
    ports: hubTls ? concat(hubPorts, panelPorts) : hubPorts
    environmentVariables: hubEnv
    // identity: /data is the image's own directory (owned by nonroot)
    volumeMounts: useKey ? [{ name: 'state', mountPath: '/data' }] : []
    // restore + bootstrap run before the hub listens
    livenessProbe: union(probe, { initialDelaySeconds: 180, periodSeconds: 30, timeoutSeconds: 10, failureThreshold: 4 })
    readinessProbe: union(probe, { initialDelaySeconds: 5, periodSeconds: 10, timeoutSeconds: 10, failureThreshold: 3 })
  }
}

var caddyContainer = {
  name: 'caddy'
  properties: {
    image: caddyImage
    command: [
      'caddy'
      'run'
      '--config'
      '/etc/caddy/Caddyfile'
      '--adapter'
      'caddyfile'
    ]
    resources: {
      requests: {
        cpu: json('0.25')
        memoryInGB: json('0.3') // ACI: steps of 0.1 GB
      }
    }
    ports: [
      { port: 80, protocol: 'TCP' }
      { port: 443, protocol: 'TCP' }
    ]
    volumeMounts: [
      { name: 'caddyfile', mountPath: '/etc/caddy', readOnly: true }
      { name: 'caddy', mountPath: '/data' }
    ]
  }
}

var baseVolumes = !useKey ? [] : [
  {
    name: 'state'
    azureFile: {
      shareName: stateShare.name
      storageAccountName: storage.name
      storageAccountKey: storageKey
    }
  }
]
var panelVolumes = [
  {
    name: 'caddy'
    azureFile: {
      shareName: 'caddy'
      storageAccountName: storage.name
      storageAccountKey: storageKey
    }
  }
  {
    name: 'caddyfile'
    secret: {
      Caddyfile: base64(caddyfileText)
    }
  }
]

var panelPorts = [
  { port: 80, protocol: 'TCP' }
  { port: 443, protocol: 'TCP' }
]

resource group 'Microsoft.ContainerInstance/containerGroups@2025-09-01' = {
  name: name
  location: location
  tags: tags
  identity: useKey ? null : {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identity.id}': {}
    }
  }
  // identity: the roles before the first start (the restore runs right away)
  dependsOn: [
    caddyShare
    replicaRole
    acrRole
    diagRole
    telemetryRole
  ]
  properties: {
    osType: 'Linux'
    sku: 'Standard'
    restartPolicy: 'Always'
    containers: enablePanel && useKey ? [hubContainer, caddyContainer] : [hubContainer]
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
      dnsNameLabel: name
      ports: enablePanel ? concat(hubPorts, panelPorts) : hubPorts
    }
    volumes: enablePanel && useKey ? concat(baseVolumes, panelVolumes) : baseVolumes
    diagnostics: enableLogAnalytics ? {
      logAnalytics: {
        workspaceId: logs!.properties.customerId
        workspaceKey: logs!.listKeys().primarySharedKey
      }
    } : null
  }
}

@description('Hub address for longarm join / longarmd join.')
output hubAddress string = hubAddress
output fqdn string = group.properties.ipAddress.fqdn
output ipAddress string = group.properties.ipAddress.ip
@description('Web panel and install scripts (empty without the panel).')
output panelUrl string = enablePanel ? 'https://${fqdn}' : ''
output storageAccount string = storage.name
@description('UDP port of the longnet relay (workers: --listen longnet://<fqdn>:<port>).')
output relayUdpPort int = relayUdpPort
output containerGroup string = group.name
@description('storageAuth identity: the managed identity\'s principal ID (empty with key).')
output identityPrincipalId string = useKey ? '' : identity!.properties.principalId
@description('Application Insights resource (empty without enableTelemetry).')
output appInsights string = enableTelemetry ? appInsights.name : ''
@description('Blob URL of the diagnostics container (empty without it).')
output diagnosticsUrl string = useDiag ? '${storage.properties.primaryEndpoints.blob}diagnostics' : ''
