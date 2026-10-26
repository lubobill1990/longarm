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
# Re-running is an upgrade: the template is applied again with the running
# hub's settings (image, size, panel, ...) unless an option changes them,
# same storage, same data, and the hub restarts.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# the Azure CLI; tests put a fake here (e2e/azure_deploy_test.go)
AZ=${LONGARM_DEPLOY_AZ:-az}
az() { if [[ $AZ == az ]]; then command az "$@"; else "$AZ" "$@"; fi; }
template=$here/azuredeploy.json

# empty = not given (a re-run keeps the running hub's value)
name='' location='' size='' panel='' log_analytics='' rg='' image='' teardown=false yes=false
recreate=false admin_code=false subscription='' acme_email='' registry_user='' tailscale_secret_file=''
timeout=900

usage() {
	cat <<'EOF'
usage: deploy.sh --name NAME [--location REGION] [options]

  --name NAME              hub name = DNS label: NAME.REGION.azurecontainer.io
  --location REGION        Azure region, e.g. eastasia, westeurope (needed on the first run)
  --size small|medium      small = 1 vCPU / 1.5 GB (default), medium = 2 vCPU / 3 GB
  --no-panel / --panel     without / with the HTTPS panel (Caddy sidecar on 80/443; default with)
  --resource-group RG      default: NAME-rg
  --image IMAGE            longarm-hub image (first run: the template's :latest; re-run: the running one)
  --registry-user USER     private image: registry login, password in $LONGARM_REGISTRY_PASSWORD
  --tailscale-secret-file F  Tailscale OAuth client secret for the hub's tailnet provider
  --acme-email EMAIL       Let's Encrypt contact for the panel certificate
  --log-analytics          send container logs to a Log Analytics workspace
  --subscription ID        Azure subscription (default: az account show)
  --recreate               delete the container group first (needed to change --size or the panel)
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
	--panel) panel=true ;;
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
[[ -z $size || $size == small || $size == medium ]] || die "--size: small or medium"
[[ $timeout =~ ^[0-9]+$ ]] || die "--timeout: seconds"
[[ -z $tailscale_secret_file || -r $tailscale_secret_file ]] || die "--tailscale-secret-file: can't read $tailscale_secret_file"
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

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 700 "$tmp"

# fetch_bootstrap ACCOUNT KEY: bootstrap.env from the state share into
# $tmp/bootstrap.env (the key goes through the environment, not argv)
fetch_bootstrap() {
	rm -f "$tmp/bootstrap.env"
	AZURE_STORAGE_KEY=$2 az storage file download --account-name "$1" --share-name state \
		--path bootstrap.env --dest "$tmp/bootstrap.env" --no-progress -o none >/dev/null 2>&1 &&
		[[ -s $tmp/bootstrap.env ]]
}

# bootstrap_value KEY: a value of $tmp/bootstrap.env
bootstrap_value() { sed -n "s/^$1=//p" "$tmp/bootstrap.env" | tr -d '\r' | head -n 1; }

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
	# az container exec runs one program without arguments: an alias of
	# longarm-hub join-code --role admin --ttl 24h (cmd/longarm-hub/container.go)
	echo "A new one-time admin join code (valid 24 h):" >&2
	# MSYS_NO_PATHCONV: Git Bash on Windows would turn the path into C:/Program Files/Git/usr/...
	MSYS_NO_PATHCONV=1 az container exec -g "$rg" -n "$name" --container-name hub --exec-command /usr/local/bin/longarm-hub-admin-code
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

# A re-run keeps the running hub's settings. Secrets can't be read back
# from ACI: a hub that has them needs them again.
cur=$(az container show -g "$rg" -n "$name" -o tsv --query "[containers[0].image, containers[0].resources.requests.cpu, containers[1].name, imageRegistryCredentials[0].server, join(',', containers[0].environmentVariables[].name), diagnostics.logAnalytics.workspaceId]" 2>/dev/null || true)
cur_image='' prev_boot='' account='' key=''
if [[ -n $cur ]]; then
	# one field per line; null fields are empty lines
	{
		read -r cur_image
		read -r cur_cpu
		read -r cur_c1
		read -r cur_registry
		read -r cur_env
		read -r cur_logs || true
	} <<<"$(printf '%s\n' "$cur" | tr -d '\r' | tr '\t' '\n')"
	cur_size=small
	[[ ${cur_cpu%%.*} -lt 2 ]] || cur_size=medium
	cur_panel=false
	[[ $cur_c1 != caddy ]] || cur_panel=true
	if ! $recreate; then
		[[ -z $size || $size == "$cur_size" ]] ||
			die "the hub is $cur_size; ACI can't resize a running container group: add --recreate (data is kept)"
		[[ -z $panel || $panel == "$cur_panel" ]] ||
			die "the panel is $([[ $cur_panel == true ]] && echo on || echo off); ACI can't change the ports of a running group: add --recreate (data is kept)"
	fi
	image=${image:-$cur_image}
	size=${size:-$cur_size}
	panel=${panel:-$cur_panel}
	[[ -n $log_analytics || -z $cur_logs ]] || log_analytics=true
	[[ -z $cur_registry || -n $registry_user ]] ||
		die "the hub pulls its image with a login for $cur_registry, which can't be read back: pass --registry-user again (password in LONGARM_REGISTRY_PASSWORD)"
	[[ ,$cur_env, != *,LONGARM_HUB_TAILSCALE_OAUTH_SECRET,* || -n $tailscale_secret_file ]] ||
		die "the hub has a Tailscale OAuth secret, which can't be read back: pass --tailscale-secret-file again"
	# this start's bootstrap.env must differ from the running one's (boot_id)
	account=$(az storage account list -g "$rg" --query "[?starts_with(name, 'lahub')].name" -o tsv | tr -d '\r' | head -n 1)
	if [[ -n $account ]]; then
		key=$(az storage account keys list -g "$rg" -n "$account" --query "[0].value" -o tsv | tr -d '\r')
		if fetch_bootstrap "$account" "$key"; then
			prev_boot=$(bootstrap_value boot_id)
		fi
	fi
fi
size=${size:-small}
panel=${panel:-true}
log_analytics=${log_analytics:-false}

if $recreate && [[ -n $cur_image ]]; then
	echo "deleting container group $name (storage and data stay)"
	az container delete -g "$rg" -n "$name" --yes -o none
fi

# parameters in a private file: no secrets on the command line. The
# location is the resource group's (its programmatic name).
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
	printf ',"size":{"value":%s}' "$(json_str "$size")"
	printf ',"enablePanel":{"value":%s}' "$panel"
	printf ',"enableLogAnalytics":{"value":%s}' "$log_analytics"
	[[ -z $image ]] || printf ',"image":{"value":%s}' "$(json_str "$image")"
	[[ -z $acme_email ]] || printf ',"acmeEmail":{"value":%s}' "$(json_str "$acme_email")"
	if [[ -n $registry_user ]]; then
		[[ -n ${LONGARM_REGISTRY_PASSWORD:-} ]] || die "--registry-user needs LONGARM_REGISTRY_PASSWORD"
		# the login is for the registry the image comes from (the template defaults to ghcr.io)
		reg_host=${image%%/*}
		if [[ $image == */* && ( $reg_host == *.* || $reg_host == *:* || $reg_host == localhost ) ]]; then
			printf ',"registryServer":{"value":%s}' "$(json_str "$reg_host")"
		fi
		printf ',"registryUsername":{"value":%s}' "$(json_str "$registry_user")"
		printf ',"registryPassword":{"value":%s}' "$(json_str "$LONGARM_REGISTRY_PASSWORD")"
	fi
	if [[ -n $tailscale_secret_file ]]; then
		printf ',"tailscaleOAuthSecret":{"value":%s}' "$(json_str "$(tr -d '\r\n' <"$tailscale_secret_file")")"
	fi
	printf '}}\n'
} >"$tmp/params.json"

echo "deploying $name to $rg ($location, $size, panel $panel${image:+, $image}); this takes a few minutes"
outputs=$(az deployment group create -g "$rg" -n longarm-hub --template-file "$template" \
	--parameters "@$tmp/params.json" -o tsv \
	--query "[properties.outputs.hubAddress.value, properties.outputs.fqdn.value, properties.outputs.storageAccount.value, properties.outputs.relayUdpPort.value, properties.outputs.panelUrl.value]")
# one value per line (az prints a list as tab-separated fields; panelUrl last: empty without the panel)
outputs=$(printf '%s\n' "$outputs" | tr -d '\r' | tr '\t' '\n')
{
	read -r hub_addr
	read -r fqdn
	read -r account
	read -r relay_port
	read -r panel_url || true
} <<<"$outputs"
[[ -n $hub_addr && -n $fqdn && -n $account ]] || die "deployment returned no outputs: $outputs"
key=$(az storage account keys list -g "$rg" -n "$account" --query "[0].value" -o tsv | tr -d '\r')

new_image=$(az container show -g "$rg" -n "$name" --query "containers[?name=='hub'].image" -o tsv | tr -d '\r')
if ! $recreate && [[ -n $cur_image && $cur_image == "$new_image" && $new_image =~ :(latest|edge)$ ]]; then
	echo "pulling $new_image again (az container restart)"
	az container restart -g "$rg" -n "$name" -o none
fi

echo "waiting for the hub at $hub_addr"
logs_hint="az container logs -g $rg -n $name --container-name hub"
deadline=$((SECONDS + timeout))
state=''
until [[ $state == Running ]]; do
	state=$(az container show -g "$rg" -n "$name" --query "containers[?name=='hub'].instanceView.currentState.state" -o tsv 2>/dev/null | tr -d '\r' || true)
	((SECONDS < deadline)) || die "the hub container is not running ($state): $logs_hint"
	[[ $state == Running ]] || sleep 10
done
# bootstrap.env of this start (a new boot_id; restart once if the update
# didn't restart the hub)
restarted=false
until fetch_bootstrap "$account" "$key" && [[ $(bootstrap_value boot_id) != "$prev_boot" ]]; do
	((SECONDS < deadline)) || die "no bootstrap.env from this start on the share: $logs_hint"
	if [[ -n $prev_boot ]] && ! $restarted && ((SECONDS > deadline - timeout + 240)); then
		echo "the hub did not restart: az container restart"
		az container restart -g "$rg" -n "$name" -o none
		restarted=true
	fi
	sleep 5
done
until tcp_open "$fqdn" "${hub_addr##*:}"; do
	((SECONDS < deadline)) || die "$hub_addr does not answer: $logs_hint"
	sleep 5
done
fp=$(bootstrap_value ca_fingerprint)
code=$(bootstrap_value admin_join_code)
expires=$(bootstrap_value admin_join_code_expires)

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
1. An admin has joined already. Another admin: ./deploy.sh --name $name --admin-code
EOF
fi
cat <<EOF

2. Each worker: a code from the laptop, then on the worker
     longarm admin join-code --role worker
     longarmd join $hub_addr <worker-code> --ca-fingerprint $fp --name worker-1
     longarm admin approve worker-1                 # on the laptop
     sudo longarmd service install --user "\$USER" && sudo longarmd service start
   Through the hub's relay (workers behind NAT): longarmd run --listen "longnet://$fqdn:${relay_port:-7443}?name=worker-1"
EOF
if [[ -n $panel_url ]]; then
	cat <<EOF
   Or, once a release is on the hub (longarm admin release publish, docs/usage.md):
     curl -fsSL $panel_url/install.sh | sh -s -- --join <worker-code>                      # Linux/macOS
     \$env:LONGARM_JOIN='<worker-code>'; irm $panel_url/install.ps1 | iex                   # Windows
   Panel login: az container exec -g $rg -n $name --container-name hub --exec-command /usr/local/bin/longarm-hub-user-add
EOF
fi
cat <<EOF

3. Your agents:
     claude mcp add longarm -- longarm mcp

Upgrade: ./deploy.sh --name $name --image <image:tag>    Remove: ./deploy.sh --name $name --teardown
EOF
