#!/usr/bin/env bash
# trusted-signing.sh: Azure Trusted Signing (Artifact Signing) for release.yml's
# Authenticode signing of the Windows binaries and MSIs (docs/ci-cd.md 2.7).
#
#   trusted-signing.sh account  <rg> <account> [region]   # provider, the account (Basic SKU, default eastus)
#   trusted-signing.sh verifier <rg> <account> [object-id]# "Identity Verifier" role (default: you) - then
#                                                         # the identity validation in the portal (2.7)
#   trusted-signing.sh ci       <rg> <account> <owner/repo>
#        an Entra app registration "<repo>-release-signing" with ONE federated credential,
#        subject repo:<owner/repo>:environment:release-signing (no secret); the role
#        "Certificate Profile Signer" on the account only; the GitHub environment
#        release-signing (deployments from v* tags only) and the repository variables
#        AZURE_CLIENT_ID, AZURE_TENANT_ID, AZURE_SUBSCRIPTION_ID, TRUSTED_SIGNING_ENDPOINT,
#        TRUSTED_SIGNING_ACCOUNT
#   trusted-signing.sh profile  <rg> <account> <profile> <identity-validation-id> <owner/repo>
#        after the validation is Completed: the Public Trust certificate profile, then the
#        variables TRUSTED_SIGNING_PROFILE (turns signing on) and TRUSTED_SIGNING_SUBJECT
#   trusted-signing.sh status   <rg> <account>
#
# Needs az (logged in, subscription selected: az account set -s ...) and, for ci/profile,
# gh with admin rights on the repository. Idempotent: re-running a step updates it.
set -euo pipefail
export MSYS2_ARG_CONV_EXCL="/subscriptions;/providers" # Git Bash: keep resource IDs as they are
API=2025-10-13
ENV_NAME=release-signing
SIGNER_ROLE="Artifact Signing Certificate Profile Signer"
VERIFIER_ROLE="Artifact Signing Identity Verifier"

die() {
	echo "trusted-signing.sh: $*" >&2
	exit 1
}
sub() { az account show --query id -o tsv; }
account_id() { echo "/subscriptions/$(sub)/resourceGroups/$1/providers/Microsoft.CodeSigning/codeSigningAccounts/$2"; }
arm() { # <method> <path> [body]
	if [[ $# -gt 2 ]]; then
		az rest --method "$1" --url "https://management.azure.com$2?api-version=$API" --body "$3"
	else
		az rest --method "$1" --url "https://management.azure.com$2?api-version=$API"
	fi
}
wait_ok() { # <path>
	local s
	for _ in $(seq 60); do
		s=$(az rest --url "https://management.azure.com$1?api-version=$API" --query properties.provisioningState -o tsv)
		case $s in
		Succeeded) return 0 ;;
		Failed | Canceled) die "$1: $s" ;;
		esac
		sleep 5
	done
	die "$1: still $s"
}

cmd=${1:-}
shift || true
case $cmd in
account)
	rg=${1:?rg} name=${2:?account} region=${3:-eastus}
	az provider register -n Microsoft.CodeSigning --wait
	id=$(account_id "$rg" "$name")
	arm put "$id" "{\"location\":\"$region\",\"properties\":{\"sku\":{\"name\":\"Basic\"}}}" >/dev/null
	wait_ok "$id"
	echo "account $name: endpoint $(az rest --url "https://management.azure.com$id?api-version=$API" --query properties.accountUri -o tsv)"
	echo "Basic SKU: billed monthly in full from today (not pro-rated). Next: $0 verifier $rg $name"
	;;
verifier)
	rg=${1:?rg} name=${2:?account} who=${3:-$(az ad signed-in-user show --query id -o tsv)}
	az role assignment create --assignee-object-id "$who" --assignee-principal-type User \
		--role "$VERIFIER_ROLE" --scope "$(account_id "$rg" "$name")" -o none
	echo "$VERIFIER_ROLE: granted. Identity validation is portal-only: docs/ci-cd.md 2.7"
	;;
ci)
	rg=${1:?rg} name=${2:?account} repo=${3:?owner/repo}
	[[ $repo == */* ]] || die "repository $repo is not owner/repo"
	id=$(account_id "$rg" "$name")
	endpoint=$(az rest --url "https://management.azure.com$id?api-version=$API" --query properties.accountUri -o tsv)
	endpoint=${endpoint%/}
	app_name="${repo#*/}-release-signing"
	app=$(az ad app list --display-name "$app_name" --query "[0].appId" -o tsv)
	if [[ -z $app ]]; then
		app=$(az ad app create --display-name "$app_name" --sign-in-audience AzureADMyOrg --query appId -o tsv)
	fi
	az ad sp show --id "$app" -o none 2>/dev/null || az ad sp create --id "$app" -o none
	sp=$(az ad sp show --id "$app" --query id -o tsv)
	# exactly one credential: the environment's subject (no branch, no pull request, no secret)
	for f in $(az ad app federated-credential list --id "$app" --query "[].id" -o tsv); do
		az ad app federated-credential delete --id "$app" --federated-credential-id "$f"
	done
	az ad app federated-credential create --id "$app" --parameters "{
		\"name\": \"github-$ENV_NAME\",
		\"issuer\": \"https://token.actions.githubusercontent.com\",
		\"subject\": \"repo:$repo:environment:$ENV_NAME\",
		\"audiences\": [\"api://AzureADTokenExchange\"],
		\"description\": \"GitHub Actions $repo, environment $ENV_NAME (v* tags): Authenticode signing\"}" -o none
	az role assignment create --assignee-object-id "$sp" --assignee-principal-type ServicePrincipal \
		--role "$SIGNER_ROLE" --scope "$id" -o none
	# the environment: deployments only from v* tags
	gh api -X PUT "repos/$repo/environments/$ENV_NAME" --input - >/dev/null <<-JSON
		{"deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}}
	JSON
	if ! gh api "repos/$repo/environments/$ENV_NAME/deployment-branch-policies" --jq '.branch_policies[] | select(.type=="tag") | .name' | grep -qx 'v\*'; then
		gh api -X POST "repos/$repo/environments/$ENV_NAME/deployment-branch-policies" -f name='v*' -f type=tag >/dev/null
	fi
	gh variable set AZURE_CLIENT_ID --repo "$repo" --body "$app"
	gh variable set AZURE_TENANT_ID --repo "$repo" --body "$(az account show --query tenantId -o tsv)"
	gh variable set AZURE_SUBSCRIPTION_ID --repo "$repo" --body "$(sub)"
	gh variable set TRUSTED_SIGNING_ENDPOINT --repo "$repo" --body "$endpoint"
	gh variable set TRUSTED_SIGNING_ACCOUNT --repo "$repo" --body "$name"
	echo "CI identity $app_name ready; signing starts once TRUSTED_SIGNING_PROFILE is set ($0 profile ...)"
	;;
profile)
	rg=${1:?rg} name=${2:?account} prof=${3:?profile} ivid=${4:?identity validation id} repo=${5:?owner/repo}
	pid="$(account_id "$rg" "$name")/certificateProfiles/$prof"
	arm put "$pid" "{\"properties\":{\"profileType\":\"PublicTrust\",\"identityValidationId\":\"$ivid\"}}" >/dev/null
	wait_ok "$pid"
	cn=$(az rest --url "https://management.azure.com$pid?api-version=$API" --query properties.commonName -o tsv 2>/dev/null || true)
	gh variable set TRUSTED_SIGNING_PROFILE --repo "$repo" --body "$prof"
	if [[ -n $cn ]]; then
		gh variable set TRUSTED_SIGNING_SUBJECT --repo "$repo" --body "$cn"
	fi
	echo "profile $prof (CN ${cn:-?}): the next v* release is Authenticode-signed"
	;;
status)
	rg=${1:?rg} name=${2:?account}
	id=$(account_id "$rg" "$name")
	az rest --url "https://management.azure.com$id?api-version=$API" --query "{sku:properties.sku.name,endpoint:properties.accountUri,state:properties.provisioningState}" -o table
	az rest --url "https://management.azure.com$id/certificateProfiles?api-version=$API" --query "value[].{profile:name,type:properties.profileType,status:properties.status,cn:properties.commonName}" -o table
	;;
*)
	sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
	;;
esac
