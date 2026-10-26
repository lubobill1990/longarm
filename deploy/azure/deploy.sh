#!/usr/bin/env bash
# deploy.sh: longarm-hub on Azure Container Instances in one command
# (deploy/azure/README.md). Needs the Azure CLI (az login); runs in Azure
# Cloud Shell as is. deploy.ps1 is the same for PowerShell.
#
#   ./deploy.sh --name myhub --location eastasia [--size small|medium] [--no-panel]
#   ./deploy.sh --name myhub --image ghcr.io/<owner>/longarm-hub:1.2.3   # upgrade (data kept)
#   ./deploy.sh --name myhub --admin-code                                # a new admin join code
#   ./deploy.sh --name myhub --teardown                                  # delete everything
#   ./deploy.sh --name myhub --location eastasia --build-in-acr          # image built from this checkout in your ACR
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
build_in_acr=false stun_servers='' timeout=1200 stuck_timeout=600
# seconds between status polls (tests make it short)
poll=${LONGARM_DEPLOY_POLL_SECONDS:-10}

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
  --build-in-acr           build the image from this checkout with az acr build in an Azure Container
                           Registry in the resource group (created once; no docker needed) and deploy it
  --tailscale-secret-file F  Tailscale OAuth client secret for the hub's tailnet provider
  --acme-email EMAIL       Let's Encrypt contact for the panel certificate
  --stun-servers LIST      external STUN servers the hub advertises (host:port,...; "none" = only the
                           hub's own; default: the template's public ones, see README "Direct connections")
  --log-analytics          send container logs to a Log Analytics workspace
  --subscription ID        Azure subscription (default: az account show)
  --recreate               delete the container group first (needed to change --size or the panel)
  --admin-code             print a new one-time admin join code (az container exec)
  --teardown               delete the resource group (asks for the name unless --yes)
  --yes                    don't ask
  --timeout SECONDS        how long to wait for the hub (default 1200)
  --stuck-timeout SECONDS  give up on a container group stuck in Creating without events (default 600)
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
	--build-in-acr) build_in_acr=true ;;
	--stun-servers) stun_servers=${2:?}; shift ;;
	--tailscale-secret-file) tailscale_secret_file=${2:?}; shift ;;
	--acme-email) acme_email=${2:?}; shift ;;
	--log-analytics) log_analytics=true ;;
	--subscription) subscription=${2:?}; shift ;;
	--recreate) recreate=true ;;
	--admin-code) admin_code=true ;;
	--teardown) teardown=true ;;
	--yes | -y) yes=true ;;
	--timeout) timeout=${2:?}; shift ;;
	--stuck-timeout) stuck_timeout=${2:?}; shift ;;
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
[[ $stuck_timeout =~ ^[0-9]+$ ]] || die "--stuck-timeout: seconds"
[[ $poll =~ ^[0-9]+$ ]] || die "LONGARM_DEPLOY_POLL_SECONDS: seconds"
! $build_in_acr || [[ -z $image ]] || die "--build-in-acr builds the image: drop --image"
[[ -z $stun_servers || $stun_servers =~ ^[A-Za-z0-9.:,\[\]-]+$ ]] || die "--stun-servers: host:port,host:port or none"
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
	# az prefers a connection string or SAS from the caller's environment over the key: drop them
	# (seen on a real run: a global AZURE_STORAGE_CONNECTION_STRING pointed at another account -> ShareNotFound)
	(
		unset AZURE_STORAGE_CONNECTION_STRING AZURE_STORAGE_SAS_TOKEN AZURE_STORAGE_ACCOUNT AZURE_STORAGE_AUTH_MODE
		AZURE_STORAGE_KEY=$2 az storage file download --account-name "$1" --share-name state \
			--path bootstrap.env --dest "$tmp/bootstrap.env" --no-progress -o none >/dev/null 2>&1
	) && [[ -s $tmp/bootstrap.env ]]
}

# bootstrap_value KEY: a value of $tmp/bootstrap.env
bootstrap_value() { sed -n "s/^$1=//p" "$tmp/bootstrap.env" | tr -d '\r' | head -n 1; }

# ensure_providers NAMESPACE...: register the resource providers the
# deployment needs (a new subscription has e.g. Microsoft.ContainerInstance
# NotRegistered: the deployment would fail with MissingSubscriptionRegistration)
ensure_providers() {
	local ns state waited
	for ns in "$@"; do
		state=$(az provider show -n "$ns" --query registrationState -o tsv 2>/dev/null | tr -d '\r' || true)
		[[ $state != Registered ]] || continue
		echo "registering the resource provider $ns (${state:-unknown}; once per subscription, a minute or two)"
		az provider register -n "$ns" -o none || die "az provider register -n $ns failed (needs Contributor on the subscription; or ask its owner to register it)"
		waited=0
		until [[ $state == Registered ]]; do
			((waited < 600)) || die "$ns is still $state after 10 min: az provider show -n $ns"
			sleep "$poll"
			waited=$((waited + poll + 1))
			state=$(az provider show -n "$ns" --query registrationState -o tsv 2>/dev/null | tr -d '\r' || true)
		done
	done
}

# registry_host IMAGE: the registry of an image reference ('' = Docker Hub)
registry_host() {
	local h=${1%%/*}
	if [[ $1 == */* && ($h == *.* || $h == *:* || $h == localhost) ]]; then
		printf '%s' "$h"
	fi
}

# acr_login NAME: registry_user / LONGARM_REGISTRY_PASSWORD from the admin
# credentials of the Azure Container Registry NAME (stdout of az, not argv)
acr_login() {
	local creds
	creds=$(az acr credential show -n "$1" -g "$rg" --query "[username, passwords[0].value]" -o tsv | tr -d '\r' | tr '\t' '\n')
	{
		read -r registry_user
		read -r LONGARM_REGISTRY_PASSWORD || true
	} <<<"$creds"
	[[ -n $registry_user && -n $LONGARM_REGISTRY_PASSWORD ]] || die "no admin credentials for the registry $1 (az acr update -n $1 --admin-enabled true)"
	export LONGARM_REGISTRY_PASSWORD
}

# check_stuck: dies when the container group has sat in Creating with
# waiting containers and no events at all for --stuck-timeout seconds.
# Seen once on a real run (both containers "Waiting", no events, 30+ min,
# while the same deployment with --recreate came up in 3.5 min).
stuck_since=''
check_stuck() {
	local info pstate hub events ev2
	info=$(az container show -g "$rg" -n "$name" -o tsv \
		--query "[provisioningState, containers[?name=='hub'].instanceView.currentState.state | [0], length(containers[].instanceView.events[] || \`[]\`), length(instanceView.events || \`[]\`)]" \
		2>/dev/null | tr -d '\r' | tr '\t' '\n') || return 0
	{
		read -r pstate
		read -r hub
		read -r events
		read -r ev2 || true
	} <<<"$info"
	events=$((${events:-0} + ${ev2:-0}))
	if [[ ($pstate == Creating || $pstate == Pending) && ($hub == '' || $hub == Waiting) && $events -eq 0 ]]; then
		stuck_since=${stuck_since:-$SECONDS}
		if ((SECONDS - stuck_since >= stuck_timeout)); then
			cat >&2 <<EOM
deploy.sh: the container group $name has been $pstate for $((SECONDS - stuck_since)) s with its
containers waiting and no events at all. That is an Azure Container Instances hiccup, not
the template (seen once: the same deployment came up in 3.5 min after a recreate).
  retry:            ./deploy.sh --name $name --recreate            (storage and data are kept)
  without the panel: ./deploy.sh --name $name --no-panel --recreate
  look:             az container show -g $rg -n $name -o table
EOM
			exit 1
		fi
	else
		stuck_since=''
	fi
}

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
ensure_providers Microsoft.ContainerInstance Microsoft.Storage

# A re-run keeps the running hub's settings. Secrets can't be read back
# from ACI: a hub that has them needs them again.
cur=$(az container show -g "$rg" -n "$name" -o tsv --query "[containers[0].image, containers[0].resources.requests.cpu, containers[1].name, imageRegistryCredentials[0].server, join(',', containers[0].environmentVariables[].name), diagnostics.logAnalytics.workspaceId]" 2>/dev/null || true)
cur_image='' cur_registry='' prev_boot='' account='' key=''
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
! $log_analytics || ensure_providers Microsoft.OperationalInsights

if $build_in_acr; then
	# the image from this checkout, built by ACR Tasks (no docker here); a
	# registry in the hub's resource group, created on the first use
	ensure_providers Microsoft.ContainerRegistry
	root=$(cd "$here/../.." && pwd)
	[[ -f $root/deploy/hub/Dockerfile && -f $root/go.mod ]] || die "--build-in-acr: run from a checkout of the repository"
	acr=$(az acr list -g "$rg" --query "[?tags.\"longarm-hub\"=='$name'].name | [0]" -o tsv | tr -d '')
	if [[ -z $acr ]]; then
		acr=lahub$(od -An -tx1 -N6 /dev/urandom | tr -d ' 
')
		echo "creating the container registry $acr in $rg (Basic, about 5 USD/month)"
		az acr create -g "$rg" -n "$acr" -l "$location" --sku Basic --admin-enabled true --tags "longarm-hub=$name" -o none
	fi
	login_server=$(az acr show -g "$rg" -n "$acr" --query loginServer -o tsv | tr -d '')
	version=$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)
	tag=build-$(date -u +%Y%m%d%H%M%S)
	echo "building $login_server/longarm-hub:$tag from $root (az acr build, a few minutes)"
	# --target acr: ACR Tasks can't parse the BuildKit stages (deploy/hub/Dockerfile)
	az acr build -g "$rg" -r "$acr" -t "longarm-hub:$tag" --target acr --platform linux/amd64 
		--build-arg "VERSION=$version" -f deploy/hub/Dockerfile "$root" ||
		die "az acr build failed (the build log is above)"
	image=$login_server/longarm-hub:$tag
fi
# an image in an Azure Container Registry of this resource group: its
# admin login (no --registry-user needed, also on re-runs)
reg_host=$(registry_host "$image")
if [[ -z $registry_user && $reg_host == *.azurecr.io ]]; then
	acr=${acr:-$(az acr list -g "$rg" --query "[?loginServer=='$reg_host'].name | [0]" -o tsv 2>/dev/null | tr -d '' || true)}
	[[ -z $acr ]] || acr_login "$acr"
fi
[[ -z $cur_registry || -n $registry_user ]] ||
	die "the hub pulls its image with a login for $cur_registry, which can't be read back: pass --registry-user again (password in LONGARM_REGISTRY_PASSWORD)"

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
		if [[ -n $reg_host ]]; then
			printf ',"registryServer":{"value":%s}' "$(json_str "$reg_host")"
		fi
		printf ',"registryUsername":{"value":%s}' "$(json_str "$registry_user")"
		printf ',"registryPassword":{"value":%s}' "$(json_str "$LONGARM_REGISTRY_PASSWORD")"
	fi
	[[ -z $stun_servers ]] || printf ',"stunServers":{"value":%s}' "$(json_str "$([[ $stun_servers == none ]] || printf %s "$stun_servers")")"
	if [[ -n $tailscale_secret_file ]]; then
		printf ',"tailscaleOAuthSecret":{"value":%s}' "$(json_str "$(tr -d '\r\n' <"$tailscale_secret_file")")"
	fi
	printf '}}\n'
} >"$tmp/params.json"

echo "deploying $name to $rg ($location, $size, panel $panel${image:+, $image}); this takes a few minutes"
deadline=$((SECONDS + timeout))
logs_hint="az container logs -g $rg -n $name --container-name hub"
# --no-wait + polling: a container group stuck in Creating would block
# "az deployment group create" for an hour (check_stuck)
deployment=longarm-hub-$(date -u +%Y%m%d%H%M%S)
az deployment group create -g "$rg" -n "$deployment" --template-file "$template" \
	--parameters "@$tmp/params.json" --no-wait -o none
dstate=''
until [[ $dstate == Succeeded ]]; do
	sleep "$poll"
	dstate=$(az deployment group show -g "$rg" -n "$deployment" --query properties.provisioningState -o tsv 2>/dev/null | tr -d '\r' || true)
	case $dstate in
	Failed | Canceled)
		az deployment operation group list -g "$rg" -n "$deployment" \
			--query "[?properties.provisioningState=='Failed'].{resource: properties.targetResource.resourceName, error: properties.statusMessage.error}" -o json >&2 || true
		die "deployment $deployment: $dstate (errors above; az deployment group show -g $rg -n $deployment)"
		;;
	esac
	check_stuck
	((SECONDS < deadline)) || die "deployment $deployment is still ${dstate:-not started} after ${timeout}s: az deployment group show -g $rg -n $deployment"
done
outputs=$(az deployment group show -g "$rg" -n "$deployment" -o tsv \
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
relay_port=${relay_port:-7443}
# the relay's TLS fallback is on the hub's TCP port (ACI: UDP 7445, TCP 7443)
relay_tcp=''
[[ $relay_port == "${hub_addr##*:}" ]] || relay_tcp="tcp=$fqdn:${hub_addr##*:}&"
key=$(az storage account keys list -g "$rg" -n "$account" --query "[0].value" -o tsv | tr -d '\r')

new_image=$(az container show -g "$rg" -n "$name" --query "containers[?name=='hub'].image" -o tsv | tr -d '\r')
if ! $recreate && [[ -n $cur_image && $cur_image == "$new_image" && $new_image =~ :(latest|edge)$ ]]; then
	echo "pulling $new_image again (az container restart)"
	az container restart -g "$rg" -n "$name" -o none
fi

echo "waiting for the hub at $hub_addr"
waited_from=$SECONDS
state=''
until [[ $state == Running ]]; do
	state=$(az container show -g "$rg" -n "$name" --query "containers[?name=='hub'].instanceView.currentState.state" -o tsv 2>/dev/null | tr -d '\r' || true)
	[[ $state != Running ]] || break
	check_stuck
	((SECONDS < deadline)) || die "the hub container is not running ($state): $logs_hint"
	sleep "$poll"
done
# bootstrap.env of this start (a new boot_id; restart once if the update
# didn't restart the hub)
restarted=false
until fetch_bootstrap "$account" "$key" && [[ $(bootstrap_value boot_id) != "$prev_boot" ]]; do
	((SECONDS < deadline)) || die "no bootstrap.env from this start on the share: $logs_hint"
	if [[ -n $prev_boot ]] && ! $restarted && ((SECONDS > waited_from + 240)); then
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
   Through the hub's relay (workers behind NAT): longarmd run --listen "longnet://$fqdn:$relay_port?${relay_tcp}name=worker-1"
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
