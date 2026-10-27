// The end-to-end test on real Azure as a scheduled Azure Container Apps job
// (deploy/azure/test/ci-job.sh -> azure-e2e.sh). Its only credential is a
// user-assigned managed identity: our test tenant refuses GitHub OIDC
// tokens, client secrets and self-signed certificates for app sign-ins, so
// the test runs where a managed identity exists - in Azure - and only ever
// runs the default branch's code (never a pull request's).
//
//   az deployment group create -g longarm-ci --template-file deploy/azure/ci/ci-job.bicep \
//     -p entraTenantId=<tenant> entraClientId=<test app id>
//   az containerapp job start -g longarm-ci -n longarm-e2e        # a run now
//
// The identity is Owner of this resource group only (azure-e2e.sh deploys a
// hub with role assignments here); hubs it deploys accept its app-only token
// as the Longarm user "ci" (--entra-apps <identity client id>=ci).

@description('Region (default: the resource group\'s).')
param location string = resourceGroup().location

@description('GitHub repository (owner/name) whose default branch is tested.')
param repo string = 'lubobill1990/longarm'

@description('Branch to test.')
param branch string = 'main'

@description('Schedule (cron, UTC).')
param schedule string = '17 19 * * *'

@description('Entra tenant of the hub sign-in under test.')
param entraTenantId string

@description('App registration (client ID) of the hub sign-in under test.')
param entraClientId string

@description('Hub image to test (default: ghcr.io/<owner>/longarm-hub:sha-<the tested commit>, published by image.yml for main).')
param hubImage string = ''

@description('Container image with bash, git (tdnf) and the Azure CLI.')
param runnerImage string = 'mcr.microsoft.com/azure-cli:latest'

@description('Optional GitHub token that may write commit statuses of the repository (fine-grained: Commit statuses read/write). Empty: results only in the job\'s logs.')
@secure()
param githubStatusToken string = ''

var tags = {
  purpose: 'longarm-ci-e2e'
}

resource logs 'Microsoft.OperationalInsights/workspaces@2023-09-01' = {
  name: 'longarm-ci-logs'
  location: location
  tags: tags
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: 30
  }
}

resource env 'Microsoft.App/managedEnvironments@2024-03-01' = {
  name: 'longarm-ci'
  location: location
  tags: tags
  properties: {
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: logs.properties.customerId
        sharedKey: logs.listKeys().primarySharedKey
      }
    }
  }
}

resource runner 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'longarm-ci-runner'
  location: location
  tags: tags
}

// Owner: azure-e2e.sh's hubs assign their own identities roles in this group
resource owner 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(resourceGroup().id, runner.id, 'owner')
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '8e3af657-a8ff-443c-a75c-2fe8c4bcb635')
    principalId: runner.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

var hasToken = !empty(githubStatusToken)

resource job 'Microsoft.App/jobs@2024-03-01' = {
  name: 'longarm-e2e'
  location: location
  tags: tags
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${runner.id}': {}
    }
  }
  dependsOn: [
    owner
  ]
  properties: {
    environmentId: env.id
    configuration: {
      triggerType: 'Schedule'
      scheduleTriggerConfig: {
        cronExpression: schedule
        parallelism: 1
        replicaCompletionCount: 1
      }
      replicaTimeout: 3600
      replicaRetryLimit: 0
      secrets: hasToken ? [
        {
          name: 'github-status-token'
          value: githubStatusToken
        }
      ] : []
    }
    template: {
      containers: [
        {
          name: 'e2e'
          image: runnerImage
          resources: {
            cpu: json('2.0')
            memory: '4Gi'
          }
          command: [
            'bash'
            '-c'
            // the default branch's script, never an older copy baked into an image
            'set -e; tdnf install -y git tar >/dev/null; git clone --depth 1 --branch "$BRANCH" "https://github.com/$REPO" /src; exec /src/deploy/azure/test/ci-job.sh'
          ]
          env: concat(
            [
              { name: 'REPO', value: repo }
              { name: 'BRANCH', value: branch }
              { name: 'AZURE_CLIENT_ID', value: runner.properties.clientId }
              { name: 'RG', value: resourceGroup().name }
              { name: 'ENTRA_TENANT', value: entraTenantId }
              { name: 'ENTRA_CLIENT', value: entraClientId }
            ],
            hasToken ? [{ name: 'GITHUB_STATUS_TOKEN', secretRef: 'github-status-token' }] : [],
            empty(hubImage) ? [] : [{ name: 'HUB_IMAGE', value: hubImage }]
          )
        }
      ]
    }
  }
}

output runnerClientId string = runner.properties.clientId
output runnerPrincipalId string = runner.properties.principalId
