#!/usr/bin/env bash
# deploy.sh: longarm-hub on Azure Container Instances in one command
# (deploy/azure/README.md). Needs the Azure CLI (az login); runs in Azure
# Cloud Shell as is. deploy.ps1 is the same for PowerShell.
#
#   ./deploy.sh --name myhub --location eastasia [--size small|medium] [--no-panel]
#   ./deploy.sh --name myhub --image ghcr.io/<owner>/longarm-hub:1.2.3   # upgrade (data kept)
#   ./deploy.sh --name myhub --admin-code                                # a new admin join code
#   ./deploy.sh --name myhub --teardown                                  # delete everything
#
# Re-running is an upgrade: the template is applied again (same storage,
# same data) and a mutable image tag (:latest, :edge) is pulled again.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# the Azure CLI; tests put a fake here (e2e/azure_deploy_test.go)
AZ=${LONGARM_DEPLOY_AZ:-az}
az() { "$AZ" "$@"; }
template=$here/azuredeploy.json

name='' location='' size=small panel=true rg='' image='' teardown=false yes=false
recreate=false admin_code=false subscription='' acme_email='' log_analytics=false
registry_user='' tailscale_secret_file='' timeout=900

usage() {
	cat <<'EOF'
usage: deploy.sh --name NAME [--location REGION] [options]

  --name NAME              hub name = DNS label: NAME.REGION.azurecontainer.io
  --location REGION        Azure region, e.g. eastasia, westeurope (needed on the first run)
  --size small|medium      small = 1 vCPU / 1.5 GB (default), medium = 2 vCPU / 3 GB
  --no-panel               no HTTPS panel (no Caddy sidecar, no ports 80/443)
  --resource-group RG      default: NAME-rg
  --image IMAGE            longarm-hub image (default: the template's, :latest)
  --registry-user USER     private image: registry login, password in $LONGARM_REGISTRY_PASSWORD
  --tailscale-secret-file F  Tailscale OAuth client secret for the hub's tailnet provider
  --acme-email EMAIL       Let's Encrypt contact for the panel certificate
  --log-analytics          send container logs to a Log Analytics workspace
  --subscription ID        Azure subscription (default: az account show)
  --recreate               delete the container group first (needed to change --size or --no-panel)
  --admin-code             print a new one-time admin join code (az container exec)
  --teardown               delete the resource group (asks for the name unless --yes)
  --yes                    don't ask
  --timeout SECONDS        how long to wait for the hub (default 900)
EOF
}

die() {
	echo "deploy.sh: $*" >&2
	exit 1
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--name) name=${2:?}; shift ;;
	--location) location=${2:?}; shift ;;
	--size) size=${2:?}; shift ;;
	--no-panel) panel=false ;;
	--resource-group) rg=${2:?}; shift ;;
	--image) image=${2:?}; shift ;;
	--registry-user) registry_user=${2:?}; shift ;;
	--tailscale-secret-file) tailscale_secret_file=${2:?}; shift ;;
	--acme-email) acme_email=${2:?}; shift ;;
	--log-analytics) log_analytics=true ;;
	--subscription) subscription=${2:?}; shift ;;
	--recreate) recreate=true ;;
	--admin-code) admin_code=true ;;
	--teardown) teardown=true ;;
	--yes | -y) yes=true ;;
	--timeout) timeout=${2:?}; shift ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown argument: $1" ;;
	esac
	shift
done

[[ -n $name ]] || { usage >&2; die "--name is required"; }
[[ $name =~ ^[a-z][a-z0-9-]{1,38}[a-z0-9]$ ]] ||
	die "--name: 3-40 lowercase letters, digits and hyphens, starting with a letter"
[[ $size == small || $size == medium ]] || die "--size: small or medium"
[[ $timeout =~ ^[0-9]+$ ]] || die "--timeout: seconds"
rg=${rg:-$name-rg}

command -v "$AZ" >/dev/null 2>&1 || die "the Azure CLI (az) is missing: https://aka.ms/azure-cli (or use Azure Cloud Shell)"
if [[ -n $subscription ]]; then
	az account set --subscription "$subscription"
fi
az account show -o none 2>/dev/null || die "not logged in: run az login"

# tcp_open HOST PORT: a TCP connect within 5 s (bash /dev/tcp; macOS has no coreutils timeout)
tcp_open() {
	(exec 3<>"/dev/tcp/$1/$2") 2>/dev/null &
	local pid=$! rc=0
	(sleep 5 && kill "$pid") >/dev/null 2>&1 &
	local killer=$!
	wait "$pid" || rc=$?
	kill "$killer" 2>/dev/null || true
	return "$rc"
}

rg_exists() { [[ $(az group exists -n "$rg" -o tsv) == true ]]; }

if $teardown; then
	rg_exists || die "resource group $rg does not exist"
	tag=$(az group show -n "$rg" --query "tags.\"longarm-hub\"" -o tsv)
	[[ $tag == "$name" ]] || die "resource group $rg was not created by deploy.sh for $name (tag longarm-hub=$tag): delete it yourself"
	if ! $yes; then
		echo "This deletes resource group $rg: the hub, its database replica, the CA and all state."
		read -r -p "Type the hub name ($name) to continue: " answer
		[[ $answer == "$name" ]] || die "aborted"
	fi
	az group delete -n "$rg" --yes
	echo "deleted $rg"
	exit 0
fi

if $admin_code; then
	echo "New one-time admin join code (valid 24 h):" >&2
	az container exec -g "$rg" -n "$name" --container-name hub \
		--exec-command "/usr/local/bin/longarm-hub join-code --role admin --ttl 24h"
	exit 0
fi

[[ -f $template ]] || die "$template is missing (run from a checkout of the repository)"

if rg_exists; then
	[[ -n $location ]] || location=$(az group show -n "$rg" --query location -o tsv)
else
	[[ -n $location ]] || die "--location is required for a new deployment (az account list-locations -o table)"
	echo "creating resource group $rg in $location"
	az group create -n "$rg" -l "$location" --tags "longarm-hub=$name" -o none
fi

prev_image=$(az container show -g "$rg" -n "$name" --query "containers[?name=='hub'].image" -o tsv 2>/dev/null || true)
if $recreate && [[ -n $prev_image ]]; then
	echo "deleting container group $name (storage and data stay)"
	az container delete -g "$rg" -n "$name" --yes -o none
	prev_image=''
fi

# parameters in a private file: no secrets on the command line
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 700 "$tmp"
json_str() {
	local s=$1
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	s=${s//$'\n'/\\n}
	s=${s//$'\r'/}
	printf '"%s"' "$s"
}
{
	printf '{"contentVersion":"1.0.0.0","parameters":{'
	printf '"name":{"value":%s}' "$(json_str "$name")"
	printf ',"location":{"value":%s}' "$(json_str "$location")"
	printf ',"size":{"value":%s}' "$(json_str "$size")"
	printf ',"enablePanel":{"value":%s}' "$panel"
	printf ',"enableLogAnalytics":{"value":%s}' "$log_analytics"
	[[ -z $image ]] || printf ',"image":{"value":%s}' "$(json_str "$image")"
	[[ -z $acme_email ]] || printf ',"acmeEmail":{"value":%s}' "$(json_str "$acme_email")"
	if [[ -n $registry_user ]]; then
		[[ -n ${LONGARM_REGISTRY_PASSWORD:-} ]] || die "--registry-user needs LONGARM_REGISTRY_PASSWORD"
		printf ',"registryUsername":{"value":%s}' "$(json_str "$registry_user")"
		printf ',"registryPassword":{"value":%s}' "$(json_str "$LONGARM_REGISTRY_PASSWORD")"
	fi
	if [[ -n $tailscale_secret_file ]]; then
		printf ',"tailscaleOAuthSecret":{"value":%s}' "$(json_str "$(tr -d '\r\n' <"$tailscale_secret_file")")"
	fi
	printf '}}\n'
} >"$tmp/params.json"

echo "deploying $name to $rg ($location, $size, panel $panel); this takes a few minutes"
outputs=$(az deployment group create -g "$rg" -n longarm-hub --template-file "$template" \
	--parameters "@$tmp/params.json" -o tsv \
	--query "[properties.outputs.hubAddress.value, properties.outputs.fqdn.value, properties.outputs.storageAccount.value, properties.outputs.panelUrl.value]")
# one value per line (az prints a list as tab-separated fields; panelUrl last: empty without the panel)
outputs=$(printf '%s\n' "$outputs" | tr -d '\r' | tr '\t' '\n')
{
	read -r hub_addr
	read -r fqdn
	read -r account
	read -r panel_url || true
} <<<"$outputs"
[[ -n $hub_addr && -n $fqdn && -n $account ]] || die "deployment returned no outputs: $outputs"

new_image=$(az container show -g "$rg" -n "$name" --query "containers[?name=='hub'].image" -o tsv)
if [[ -n $prev_image && $prev_image == "$new_image" && $new_image =~ :(latest|edge)$ ]]; then
	echo "pulling $new_image again (az container restart)"
	az container restart -g "$rg" -n "$name" -o none
fi

echo "waiting for the hub at $hub_addr"
deadline=$((SECONDS + timeout))
state=''
until [[ $state == Running ]]; do
	state=$(az container show -g "$rg" -n "$name" --query "containers[?name=='hub'].instanceView.currentState.state" -o tsv 2>/dev/null || true)
	((SECONDS < deadline)) || die "the hub container is not running ($state): az container logs -g $rg -n $name --container-name hub"
	[[ $state == Running ]] || sleep 10
done
until tcp_open "$fqdn" "${hub_addr##*:}"; do
	((SECONDS < deadline)) || die "$hub_addr does not answer: az container logs -g $rg -n $name --container-name hub"
	sleep 5
done

# bootstrap.env from the state share (else the container log)
key=$(az storage account keys list -g "$rg" -n "$account" --query "[0].value" -o tsv)
boot=''
until [[ -n $boot ]]; do
	if az storage file download --account-name "$account" --account-key "$key" --share-name state \
		--path bootstrap.env --dest "$tmp/bootstrap.env" --no-progress -o none 2>/dev/null; then
		boot=$(cat "$tmp/bootstrap.env")
	else
		boot=$(az container logs -g "$rg" -n "$name" --container-name hub 2>/dev/null |
			sed -n 's/^LONGARM_BOOTSTRAP //p' | tail -n 1 | tr ' ' '\n')
	fi
	if [[ -z $boot ]]; then
		((SECONDS < deadline)) || die "no bootstrap.env on the share yet: az container logs -g $rg -n $name --container-name hub"
		sleep 5
	fi
done
fp='' code='' expires='' joined=''
while IFS='=' read -r k v; do
	case $k in
	ca_fingerprint) fp=$v ;;
	admin_join_code) code=$v ;;
	admin_join_code_expires) expires=$v ;;
	admin_joined) joined=$v ;;
	esac
done <<<"$boot"

if [[ -n $panel_url ]]; then
	ok=false
	for _ in $(seq 1 18); do
		if curl -fsS -m 10 -o /dev/null "$panel_url/healthz" 2>/dev/null; then
			ok=true
			break
		fi
		sleep 10
	done
	$ok || echo "note: $panel_url/healthz does not answer yet (Let's Encrypt can take a few minutes): az container logs -g $rg -n $name --container-name caddy" >&2
fi

# Rough monthly cost (Linux ACI pay-as-you-go, about 0.0405 USD per vCPU-hour
# and 0.0045 USD per GB-hour in US regions; other regions differ by up to ~20 %).
cost() {
	awk -v s="$size" -v p="$panel" 'BEGIN {
		cpu = (s == "medium") ? 2 : 1; mem = (s == "medium") ? 3 : 1.5
		if (p == "true") { cpu += 0.25; mem += 0.25 }
		printf "%.0f", 730 * (cpu * 0.0405 + mem * 0.0045) + 1
	}'
}

cat <<EOF

longarm-hub is up.
  hub address     $hub_addr
  CA fingerprint  $fp
  panel           ${panel_url:-off (--no-panel)}
  resource group  $rg (storage account $account)
  cost            about $(cost) USD/month (compute; plus egress over 100 GB/month, relay traffic counts)

EOF
if [[ -n $code ]]; then
	cat <<EOF
1. Your laptop (admin): install longarm (https://github.com/lubobill1990/longarm/releases), then
     longarm join $hub_addr $code --ca-fingerprint $fp
   The code is one-time and valid until $expires (a new one: ./deploy.sh --name $name --admin-code).
EOF
else
	cat <<EOF
1. An admin has joined already (admin_joined=$joined). Another admin: ./deploy.sh --name $name --admin-code
EOF
fi
cat <<EOF

2. Each worker: a code from the laptop, then on the worker
     longarm admin join-code --role worker
     longarmd join $hub_addr <worker-code> --ca-fingerprint $fp --name worker-1
     longarm admin approve worker-1                 # on the laptop
     sudo longarmd service install --user "\$USER" && sudo longarmd service start
EOF
if [[ -n $panel_url ]]; then
	cat <<EOF
   Or, once a release is on the hub (longarm admin release publish, docs/usage.md):
     curl -fsSL $panel_url/install.sh | sh -s -- --join <worker-code>                      # Linux/macOS
     \$env:LONGARM_JOIN='<worker-code>'; irm $panel_url/install.ps1 | iex                   # Windows
   Panel login: az container exec -g $rg -n $name --container-name hub --exec-command "/usr/local/bin/longarm-hub user add admin"
EOF
fi
cat <<EOF

3. Your agents:
     claude mcp add longarm -- longarm mcp

Upgrade: ./deploy.sh --name $name --image <image:tag>    Remove: ./deploy.sh --name $name --teardown
EOF
