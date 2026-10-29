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
#   ./deploy.sh --name myhub --location eastasia --storage-auth identity # no storage account keys
#   ./deploy.sh --name myhub --headscale --recreate                      # + a Headscale sidecar (tailnet)
#
# Re-running is an upgrade: the template is applied again with the running
# hub's settings (image, size, panel, ...) unless an option changes them,
# same storage, same data, and the hub restarts.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# the Azure CLI; tests put a fake here (e2e/azure_deploy_test.go)
AZ=${LONGARM_DEPLOY_AZ:-az}
# command: "$AZ" is often "az", which would call this function again
az() { command "$AZ" "$@"; }
template=$here/azuredeploy.json

# empty = not given (a re-run keeps the running hub's value)
name='' location='' size='' panel='' log_analytics='' rg='' image='' teardown=false yes=false
recreate=false admin_code=false subscription='' acme_email='' registry_user='' tailscale_secret_file=''
entra_tenant='' entra_client='' entra_admins='' entra_web='' entra_apps='' gate_free_gb='' release_public_key=''
build_in_acr=false stun_servers='' storage_auth='' acr='' timeout=1200 stuck_timeout=600
telemetry='' diagnostics='' headscale='' public_derp='' skip_image_check=false
# seconds between status polls (tests make it short; each az call takes a
# second or two on its own)
poll=${LONGARM_DEPLOY_POLL_SECONDS:-5}

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
  --headscale / --no-headscale  a self-hosted Tailscale control plane (Headscale) as a sidecar,
                           served by the hub on https://<fqdn>; the hub's tailnet provider (needs
                           the panel; switching it on or off needs --recreate; README "Headscale")
  --headscale-public-derp on|off  also hand nodes Tailscale's public DERP relays (default off)
  --acme-email EMAIL       Let's Encrypt contact for the panel certificate
  --entra-tenant ID --entra-client ID   Sign in with Microsoft Entra ID (longarm login,
                           panel): the app registration (docs/usage.md)
  --entra-admins LIST      Entra admins: UPNs, *@domain or object IDs, comma-separated
  --entra-web MODE         panel sign-in: spa (default) | confidential | off
  --entra-apps LIST        app-only sign-in (CI): "<app id>=<user>", comma-separated
  --gate-free-gb N         scheduler memory gate: free GB a worker needs for scheduled jobs
                           (default 20; lower it for small workers such as containers or laptops)
  --release-public-key KEY the minisign public key line (RW...) releases are signed with: the hub
                           refuses uploads whose signature doesn't verify (tail -1 longarm-release.pub)
  --stun-servers LIST      external STUN servers the hub advertises (host:port,...; "none" = only the
                           hub's own; default: the template's public ones, see README "Direct connections")
  --log-analytics          send container logs to a Log Analytics workspace
  --storage-auth key|identity  key (default): storage account keys; identity: a managed
                           identity, shared key access off (subscriptions whose policy denies keys)
  --acr NAME               with identity: pull the image from this registry in the resource group
  --telemetry              Application Insights + Log Analytics for the fleet's telemetry (docs/telemetry.md)
  --diagnostics            a blob container for uploaded diagnostic bundles (identity only; 30 days)
  --subscription ID        Azure subscription (default: az account show)
  --recreate               delete the container group first (needed to change --size, the panel or
                           --headscale). Validated first, the settings saved in the resource
                           group's tags, and rolled back to the previous configuration if the new
                           one fails
  --skip-image-check       don't check that every image can be pulled before deploying
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
	--headscale) headscale=true ;;
	--no-headscale) headscale=false ;;
	--headscale-public-derp) public_derp=${2:?}; shift ;;
	--acme-email) acme_email=${2:?}; shift ;;
	--entra-tenant) entra_tenant=${2:?}; shift ;;
	--entra-client) entra_client=${2:?}; shift ;;
	--entra-admins) entra_admins=${2:?}; shift ;;
	--entra-web) entra_web=${2:?}; shift ;;
	--entra-apps) entra_apps=${2:?}; shift ;;
	--gate-free-gb) gate_free_gb=${2:?}; shift ;;
	--release-public-key) release_public_key=${2:?}; shift ;;
	--log-analytics) log_analytics=true ;;
	--storage-auth) storage_auth=${2:?}; shift ;;
	--acr) acr=${2:?}; shift ;;
	--telemetry) telemetry=true ;;
	--diagnostics) diagnostics=true ;;
	--subscription) subscription=${2:?}; shift ;;
	--recreate) recreate=true ;;
	--skip-image-check) skip_image_check=true ;;
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
# none alone, or host:port / [v6]:port entries (the hub refuses a missing
# port: the container would crash-loop until --timeout)
stun_re='^([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\]):[0-9]{1,5}$'
if [[ -n $stun_servers && $stun_servers != none ]]; then
	IFS=, read -ra stun_list <<<"$stun_servers"
	[[ ${#stun_list[@]} -gt 0 && $stun_servers != *, ]] || die "--stun-servers: host:port,host:port or none"
	for s in "${stun_list[@]}"; do
		[[ $s =~ $stun_re ]] || die "--stun-servers: $s is not host:port or [v6]:port (none goes alone)"
	done
fi
[[ -z $storage_auth || $storage_auth == key || $storage_auth == identity ]] || die "--storage-auth: key or identity"
# the key line of a minisign .pub file, not the file or its comment line
[[ -z $release_public_key || $release_public_key =~ ^RW[A-Za-z0-9+/=]{40,}$ ]] ||
	die "--release-public-key: the minisign public key line (RW...), e.g. \"\$(tail -1 longarm-release.pub)\""
[[ -z $tailscale_secret_file || -r $tailscale_secret_file ]] || die "--tailscale-secret-file: can't read $tailscale_secret_file"
case $public_derp in
'') ;;
on) public_derp=true ;;
off) public_derp=false ;;
*) die "--headscale-public-derp: on or off" ;;
esac
rg=${rg:-$name-rg}

command -v "$AZ" >/dev/null 2>&1 || die "the Azure CLI (az) is missing: https://aka.ms/azure-cli (or use Azure Cloud Shell)"
if [[ -n $subscription ]]; then
	az account set --subscription "$subscription"
fi
az account show -o none 2>/dev/null || die "not logged in: run az login"

# tcp_open HOST PORT: a TCP connect within 5 s (bash /dev/tcp; macOS has no coreutils timeout)
tcp_open() {
	(trap - EXIT; exec 3<>"/dev/tcp/$1/$2") 2>/dev/null &
	local pid=$! rc=0
	(trap - EXIT; sleep 5 && kill "$pid") >/dev/null 2>&1 &
	local killer=$!
	wait "$pid" || rc=$?
	kill "$killer" 2>/dev/null || true
	return "$rc"
}

rg_exists() { [[ $(az group exists -n "$rg" -o tsv) == true ]]; }

tmp=$(mktemp -d)
# only the script itself cleans up: a background subshell that is killed
# (tcp_open) runs an inherited EXIT trap too, and took $tmp with it. bash
# 3.2 (macOS) has no BASHPID: there tcp_open's trap - EXIT does it alone
main_pid=${BASHPID:-$$}
trap '[[ ${BASHPID:-$$} != "$main_pid" ]] || rm -rf "$tmp"' EXIT
chmod 700 "$tmp"

# fetch_bootstrap ACCOUNT KEY: bootstrap.env from the state share into
# $bootstrap (the key goes through the environment, not argv; the file with
# the admin join code is removed right away)
bootstrap=''
fetch_bootstrap() {
	bootstrap=''
	rm -f "$tmp/bootstrap.env"
	# az prefers a connection string or SAS from the caller's environment over the key: drop them
	# (seen on a real run: a global AZURE_STORAGE_CONNECTION_STRING pointed at another account -> ShareNotFound)
	(
		unset AZURE_STORAGE_CONNECTION_STRING AZURE_STORAGE_SAS_TOKEN AZURE_STORAGE_ACCOUNT AZURE_STORAGE_AUTH_MODE
		AZURE_STORAGE_KEY=$2 az storage file download --account-name "$1" --share-name state \
			--path bootstrap.env --dest "$tmp/bootstrap.env" --no-progress -o none >/dev/null 2>&1
	) || return 1
	bootstrap=$(tr -d '\r' <"$tmp/bootstrap.env" 2>/dev/null) || true
	rm -f "$tmp/bootstrap.env"
	[[ -n $bootstrap ]]
}

# fetch_bootstrap_log: the last LONGARM_BOOTSTRAP line of the hub's log
# into $bootstrap (storage-auth identity: no share; the line has no join code)
fetch_bootstrap_log() {
	bootstrap=$(az container logs -g "$rg" -n "$name" --container-name hub 2>/dev/null | tr -d '\r' |
		grep '^LONGARM_BOOTSTRAP ' | tail -n 1 | tr ' ' '\n' | grep '=') || true
	[[ -n $bootstrap ]]
}

# read_bootstrap: this hub's bootstrap values into $bootstrap
read_bootstrap() {
	if [[ $storage_auth == identity ]]; then
		fetch_bootstrap_log
	else
		fetch_bootstrap "$account" "$key"
	fi
}

# bootstrap_value KEY: a value of the last fetched bootstrap values (awk reads
# to the end: an early exit can SIGPIPE printf, and pipefail + set -e end the script)
bootstrap_value() { printf '%s\n' "$bootstrap" | awk -v k="$1" '!found && index($0, k "=") == 1 { print substr($0, length(k) + 2); found = 1 }'; }

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

# check_stuck: 1 (with what to do on stderr) when the container group has sat in Creating with
# waiting containers and no events at all for --stuck-timeout seconds.
# Seen once on a real run (both containers "Waiting", no events, 30+ min,
# while the same deployment with --recreate came up in 3.5 min).
stuck_since=''
check_stuck() {
	local info pstate hub events ev2
	info=$(az container show -g "$rg" -n "$name" -o tsv \
		--query "[provisioningState, containers[?name=='hub'].instanceView.currentState.state | [0], length(containers[].instanceView.events[] || \`[]\`), length(instanceView.events || \`[]\`)]" \
		2>/dev/null | tr -d '\r' | tr '\t' '\n' | sed 's/^None$//') || return 0
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
			return 1
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

# A re-run keeps the hub's settings (prev_*). Their source, in this order:
#   1. the container group (the running hub's image and environment)
#   2. the snapshot this script tags the resource group with before it
#      deletes the group (--recreate), until a deployment succeeds again
#   3. the parameters of the last successful deployment (ARM keeps them)
# Without any of them, a resource group that has a hub's storage needs the
# settings given explicitly: defaulting would switch storage auth or drop
# the identity (seen on a real run: a retry after a failed --recreate fell
# back to storage keys, which the subscription's policy denies).
# Secrets can't be read back from any of them: a hub that has them needs
# them again.
prev_src='' prev_image='' prev_size='' prev_panel='' prev_auth='' prev_acr='' prev_logs='' prev_telemetry=''
prev_diagnostics='' prev_stun='' prev_gate='' prev_relkey='' prev_entra_tenant='' prev_entra_client=''
prev_entra_admins='' prev_entra_web='' prev_entra_apps='' prev_headscale='' prev_public_derp=''
prev_registry='' prev_registry_user='' prev_tailscale=''
prev_keys='image size panel auth acr logs telemetry diagnostics stun gate relkey entra_tenant entra_client entra_admins entra_web entra_apps headscale public_derp registry registry_user tailscale'

# load_prev_group: prev_* from the container group (1 = there is none)
load_prev_group() {
	local cur cur_cpu cur_c1 cur_env cur_logs cur_stun k v
	cur=$(az container show -g "$rg" -n "$name" -o tsv --query "[containers[0].image, containers[0].resources.requests.cpu, containers[1].name, imageRegistryCredentials[0].server, join(',', containers[0].environmentVariables[].name), diagnostics.logAnalytics.workspaceId, containers[0].environmentVariables[?name=='LONGARM_HUB_RELAY_STUN_ADVERTISE'].value | [0]]" 2>/dev/null || true)
	[[ -n $cur ]] || return 1
	# one field per line; null fields are empty lines (the real az prints
	# "None" for them in a tsv list: a hub without registry credentials)
	{
		read -r prev_image
		read -r cur_cpu
		read -r cur_c1
		read -r prev_registry
		read -r cur_env
		read -r cur_logs || true
		read -r cur_stun || true
	} <<<"$(printf '%s\n' "$cur" | tr -d '\r' | tr '\t' '\n' | sed 's/^None$//')"
	prev_size=small
	[[ ${cur_cpu%%.*} -lt 2 ]] || prev_size=medium
	prev_auth=key
	[[ ,$cur_env, != *,LONGARM_HUB_CA_IN_DB,* ]] || prev_auth=identity
	# the panel: Caddy (storage-auth key) or the hub on 443 (identity)
	prev_panel=false
	[[ $cur_c1 != caddy && ,$cur_env, != *,LONGARM_HUB_HTTPS,* ]] || prev_panel=true
	# the registry pulled with the identity (identity mode)
	[[ $prev_auth != identity || $prev_registry != *.azurecr.io ]] || prev_acr=${prev_registry%%.*}
	flag_of() { if [[ ,$cur_env, == *,$1,* ]]; then echo true; else echo false; fi; }
	prev_logs=false
	[[ -z $cur_logs ]] || prev_logs=true
	prev_telemetry=$(flag_of LONGARM_HUB_APPINSIGHTS_CONNECTION_STRING)
	prev_diagnostics=$(flag_of LONGARM_HUB_DIAG_BLOB_URL)
	prev_headscale=$(flag_of LONGARM_HUB_HEADSCALE_DIR)
	prev_public_derp=$(flag_of LONGARM_HUB_HEADSCALE_PUBLIC_DERP)
	prev_tailscale=$(flag_of LONGARM_HUB_TAILSCALE_OAUTH_SECRET)
	# the template sets self[,<stunServers>]; absent = a hub from before (template default)
	case $cur_stun in
	self) prev_stun=none ;;
	self,*) prev_stun=${cur_stun#self,} ;;
	esac
	# plain environment values: the scheduler gate, the release key, Entra
	while IFS=$'\t' read -r k v; do
		[[ $v != None ]] || v=''
		case $k in
		LONGARM_HUB_SCHED_GATE_FREE_GB) prev_gate=$v ;;
		LONGARM_HUB_RELEASES_PUBLIC_KEY) prev_relkey=$v ;;
		LONGARM_HUB_ENTRA_TENANT_ID) prev_entra_tenant=$v ;;
		LONGARM_HUB_ENTRA_CLIENT_ID) prev_entra_client=$v ;;
		LONGARM_HUB_ENTRA_ADMINS) prev_entra_admins=$v ;;
		LONGARM_HUB_ENTRA_WEB) prev_entra_web=$v ;;
		LONGARM_HUB_ENTRA_APPS) prev_entra_apps=$v ;;
		esac
	done < <(az container show -g "$rg" -n "$name" -o tsv \
		--query "containers[0].environmentVariables[?starts_with(name, 'LONGARM_HUB_ENTRA_') || name=='LONGARM_HUB_SCHED_GATE_FREE_GB' || name=='LONGARM_HUB_RELEASES_PUBLIC_KEY'].[name, value]" 2>/dev/null | tr -d '\r' || true)
	prev_src=group
}

# prev_text: the prev_* settings as key=value lines
prev_text() {
	local k v
	for k in $prev_keys; do
		v=prev_$k
		printf '%s=%s\n' "$k" "${!v}"
	done
}

# prev_parse PREFIX TEXT: <PREFIX><key> from prev_text's lines
prev_parse() {
	local line k
	while IFS= read -r line; do
		k=${line%%=*}
		[[ " $prev_keys " == *" $k "* ]] || continue
		printf -v "$1$k" '%s' "${line#*=}"
	done <<<"$2"
}

# The snapshot: prev_text, base64, in resource group tags of 250 characters
# (longarm-hub-prev-0..N-1; longarm-hub-prev-n = N, 0 = none). Tags can be
# written by anyone with Tag Contributor, so the snapshot is not trusted: it
# only fills settings the last deployment's parameters don't have, and its
# security-relevant settings (sec_keys) are refused when they differ from
# that deployment's or there is no deployment to compare them with, unless
# the operator gives them explicitly.
sec_keys='image auth acr registry_user relkey entra_tenant entra_client entra_admins entra_web entra_apps'
# the option that gives each security-relevant setting explicitly
sec_option() {
	case $1 in
	image) echo "--image" ;; auth) echo "--storage-auth" ;; acr) echo "--acr" ;;
	registry | registry_user) echo "--registry-user" ;; relkey) echo "--release-public-key" ;;
	entra_tenant) echo "--entra-tenant" ;; entra_client) echo "--entra-client" ;; entra_admins) echo "--entra-admins" ;;
	entra_web) echo "--entra-web" ;; entra_apps) echo "--entra-apps" ;; stun) echo "--stun-servers" ;;
	esac
}
# sec_given KEY: the operator gave that setting on the command line
sec_given() {
	case $1 in
	image) [[ -n $image ]] ;; auth) [[ -n $storage_auth ]] ;; acr) [[ -n $acr ]] ;;
	registry | registry_user) [[ -n $registry_user ]] ;; relkey) [[ -n $release_public_key ]] ;;
	entra_tenant) [[ -n $entra_tenant ]] ;; entra_client) [[ -n $entra_client ]] ;; entra_admins) [[ -n $entra_admins ]] ;;
	entra_web) [[ -n $entra_web ]] ;; entra_apps) [[ -n $entra_apps ]] ;; stun) [[ -n $stun_servers ]] ;;
	esac
}
# template_default PARAM: a parameter's default in azuredeploy.json
template_default() {
	[[ -n $1 ]] || return 0
	sed -n "/^    \"$1\": {/,/^    }/s/^ *\"defaultValue\": \"\(.*\)\",\{0,1\}\$/\1/p" "$template" | head -n 1
}
rg_id=''
snapshot_tags() {
	local b64 i n tags=()
	b64=$(prev_text | base64 | tr -d '\n\r')
	n=$(((${#b64} + 249) / 250))
	for ((i = 0; i < n; i++)); do
		tags+=("longarm-hub-prev-$i=${b64:i*250:250}")
	done
	tags+=("longarm-hub-prev-n=$n" "longarm-hub-prev-at=$(date -u +%Y-%m-%dT%H:%M:%SZ)")
	rg_id=${rg_id:-$(az group show -n "$rg" --query id -o tsv | tr -d '\r')}
	az tag update --resource-id "$rg_id" --operation Merge --tags "${tags[@]}" -o none
}
clear_snapshot() {
	rg_id=${rg_id:-$(az group show -n "$rg" --query id -o tsv | tr -d '\r')}
	az tag update --resource-id "$rg_id" --operation Merge --tags "longarm-hub-prev-n=0" -o none 2>/dev/null ||
		echo "note: the settings snapshot (resource group tag longarm-hub-prev-n) was not cleared" >&2
}
# load_snapshot: snap_* from the tags (1 = none)
have_snapshot=false
load_snapshot() {
	local n i b64='' text
	n=$(az group show -n "$rg" --query 'tags."longarm-hub-prev-n"' -o tsv 2>/dev/null | tr -d '\r' || true)
	[[ $n =~ ^[1-9][0-9]*$ ]] || return 1
	for ((i = 0; i < n; i++)); do
		b64+=$(az group show -n "$rg" --query "tags.\"longarm-hub-prev-$i\"" -o tsv | tr -d '\r')
	done
	text=$(printf '%s' "$b64" | base64 -d 2>/dev/null) || return 1
	[[ $text == *auth=* ]] || return 1
	prev_parse snap_ "$text"
	have_snapshot=true
}

# load_prev_deployment: prev_* from the parameters of the last successful
# deployment of this script; hist_has lists the settings it has (absent
# parameters: the template's defaults, or an older script)
hist_has=''
load_prev_deployment() {
	local dep vals p k kind q='' i=0 v
	dep=$(az deployment group list -g "$rg" -o tsv \
		--query "[?properties.provisioningState=='Succeeded' && starts_with(name, 'longarm-hub-')].name" 2>/dev/null |
		tr -d '\r' | sort | tail -n 1 || true)
	[[ -n $dep ]] || return 1
	# key:parameter:kind
	local list='image:image:s size:size:s panel:enablePanel:b auth:storageAuth:s acr:acrName:s logs:enableLogAnalytics:b telemetry:enableTelemetry:b diagnostics:enableDiagnostics:b stun:stunServers:stun gate:schedulerGateFreeGb:s relkey:releasePublicKey:s entra_tenant:entraTenantId:s entra_client:entraClientId:s entra_admins:entraAdmins:s entra_web:entraWeb:s entra_apps:entraApps:s headscale:enableHeadscale:b public_derp:headscalePublicDerp:b registry:registryServer:s registry_user:registryUsername:s'
	for k in $list; do
		p=${k#*:}
		p=${p%%:*}
		q+="${q:+, }properties.parameters.$p.value, properties.parameters.$p == null"
	done
	vals=$(az deployment group show -g "$rg" -n "$dep" -o tsv --query "[$q]" 2>/dev/null | tr -d '\r' | tr '\t' '\n' | sed 's/^None$//') || return 1
	local -a f=()
	while IFS= read -r v; do f+=("$v"); done <<<"$vals"
	for k in $list; do
		kind=${k##*:}
		p=${k#*:}
		printf -v "param_${k%%:*}" '%s' "${p%%:*}"
		v=${f[i]:-}
		# absent: "<p> == null" is True
		if [[ ${f[i + 1]:-} != [Tt]rue ]]; then
			hist_has+=" ${k%%:*}"
			case $kind in
			b) case $v in [Tt]rue) v=true ;; *) v=false ;; esac ;;
			stun) [[ -n $v ]] || v=none ;; # "" = none
			esac
			printf -v "prev_${k%%:*}" '%s' "$v"
		fi
		i=$((i + 2))
	done
	# the template's defaults where the deployment had no parameter
	[[ -n $prev_size ]] || prev_size=small
	for k in panel logs telemetry diagnostics headscale public_derp; do
		v=prev_$k
		[[ -n ${!v} ]] || printf -v "$v" '%s' false
	done
	[[ $prev_auth == key || $prev_auth == identity ]] || return 1
	prev_tailscale=''
	prev_src="deployment $dep"
}

# The source of the settings: the container group, else the last successful
# deployment, with gaps filled from the snapshot (checked as above), else
# the snapshot alone for what isn't security-relevant (those settings must
# then be given), else nothing (a resource group with a hub's storage needs
# the settings given explicitly).
if load_prev_group; then
	:
else
	load_snapshot || true
	if load_prev_deployment; then
		echo "the container group $name is missing: its settings come from the last successful deployment (${prev_src#deployment })"
		if $have_snapshot; then
			filled='' refused=''
			for k in $prev_keys; do
				s=snap_$k p=prev_$k
				if [[ " $hist_has " != *" $k "* ]]; then
					# the deployment had the template's default
					d=param_$k
					if [[ " $sec_keys " == *" $k "* && -n ${!s} && ${!s} != "$(template_default "${!d:-}")" ]] && ! sec_given "$k"; then
						refused+=" $(sec_option "$k") (deployment: the template's default, snapshot: ${!s})"
					elif [[ " $sec_keys " != *" $k "* ]]; then
						printf -v "$p" '%s' "${!s}"
						filled+=" $k"
					fi
				elif [[ " $sec_keys " == *" $k "* && ${!s} != "${!p}" ]] && ! sec_given "$k"; then
					refused+=" $(sec_option "$k") (deployment: ${!p:-none}, snapshot: ${!s:-none})"
				fi
			done
			[[ -z $refused ]] ||
				die "the settings snapshot in the resource group's tags (longarm-hub-prev-*) differs from the last deployment in security-relevant settings - it may have been tampered with (tags are writable by Tag Contributors). Give them explicitly if the snapshot is right:$refused"
			[[ -z $filled ]] || echo "settings the deployment had no parameter for, from the snapshot:$filled"
		fi
	elif $have_snapshot; then
		echo "the container group $name is missing and there is no successful deployment: settings that aren't security-relevant come from the snapshot taken before it was deleted"
		missing=''
		for k in $prev_keys; do
			s=snap_$k
			if [[ " $sec_keys " == *" $k "* ]]; then
				[[ -z ${!s} ]] || sec_given "$k" || missing+=" $(sec_option "$k") (snapshot: ${!s})"
			else
				printf -v "prev_$k" '%s' "${!s}"
			fi
		done
		[[ -z $missing ]] ||
			die "the settings snapshot (resource group tags, writable by Tag Contributors) can't be checked against a deployment: give its security-relevant settings explicitly if they are right:$missing"
		prev_auth=$storage_auth
		prev_src=snapshot
	elif [[ -z $storage_auth ]] && [[ -n $(az storage account list -g "$rg" --query "[?starts_with(name, 'lahub')].name" -o tsv 2>/dev/null | tr -d '\r') ]]; then
		die "the resource group $rg has a hub's storage, but neither a container group, a successful deployment nor a settings snapshot to read its settings from: pass them (at least --storage-auth key|identity; and --image, --entra-*, --telemetry, --diagnostics, --headscale, --release-public-key, --gate-free-gb, --stun-servers as the hub had them)"
	fi
fi

cur_image='' prev_boot='' account='' key='' build_acr=''
if [[ -n $prev_src ]]; then
	[[ -z $storage_auth || $storage_auth == "$prev_auth" ]] ||
		die "the hub uses --storage-auth $prev_auth; switching needs a new deployment (another --name or --resource-group)"
	storage_auth=$prev_auth
	[[ -n $acr ]] || acr=$prev_acr
	if [[ $prev_src == group ]]; then
		cur_image=$prev_image
		if ! $recreate; then
			[[ -z $size || $size == "$prev_size" ]] ||
				die "the hub is $prev_size; ACI can't resize a running container group: add --recreate (data is kept)"
			[[ -z $panel || $panel == "$prev_panel" ]] ||
				die "the panel is $([[ $prev_panel == true ]] && echo on || echo off); ACI can't change the ports of a running group: add --recreate (data is kept)"
			[[ -z $headscale || $headscale == "$prev_headscale" ]] ||
				die "the Headscale sidecar is $([[ $prev_headscale == true ]] && echo on || echo off); ACI can't add or remove containers of a running group: add --recreate (data is kept)"
		fi
	fi
	image=${image:-$prev_image}
	size=${size:-$prev_size}
	panel=${panel:-$prev_panel}
	headscale=${headscale:-$prev_headscale}
	public_derp=${public_derp:-$prev_public_derp}
	log_analytics=${log_analytics:-$prev_logs}
	telemetry=${telemetry:-$prev_telemetry}
	diagnostics=${diagnostics:-$prev_diagnostics}
	stun_servers=${stun_servers:-$prev_stun}
	gate_free_gb=${gate_free_gb:-$prev_gate}
	release_public_key=${release_public_key:-$prev_relkey}
	if [[ -z $entra_tenant ]]; then
		entra_tenant=$prev_entra_tenant
		entra_client=${entra_client:-$prev_entra_client}
		entra_admins=${entra_admins:-$prev_entra_admins}
		entra_web=${entra_web:-$prev_entra_web}
		entra_apps=${entra_apps:-$prev_entra_apps}
	fi
	[[ $prev_tailscale != true || -n $tailscale_secret_file || $headscale == true ]] ||
		die "the hub has a Tailscale OAuth secret, which can't be read back: pass --tailscale-secret-file again"
	# this start's bootstrap.env must differ from the running one's (boot_id)
	if [[ $prev_src == group ]]; then
		if [[ $storage_auth == identity ]]; then
			if fetch_bootstrap_log; then
				prev_boot=$(bootstrap_value boot_id)
			fi
		else
			account=$(az storage account list -g "$rg" --query "[?starts_with(name, 'lahub')].name" -o tsv | tr -d '\r' | sed -n 1p)
			if [[ -n $account ]]; then
				key=$(az storage account keys list -g "$rg" -n "$account" --query "[0].value" -o tsv | tr -d '\r')
				if fetch_bootstrap "$account" "$key"; then
					prev_boot=$(bootstrap_value boot_id)
				fi
			fi
		fi
	fi
fi
storage_auth=${storage_auth:-key}
[[ ${diagnostics:-false} != true || $storage_auth == identity ]] || die "--diagnostics needs --storage-auth identity (the hub writes bundles with its managed identity)"
[[ -z $gate_free_gb || $gate_free_gb =~ ^[0-9]+([.][0-9]+)?$ ]] || die "--gate-free-gb: a number of GB"
[[ -z $entra_tenant && -z $entra_client ]] || [[ -n $entra_tenant && -n $entra_client ]] ||
	die "--entra-tenant and --entra-client go together"
[[ -z $entra_web || $entra_web =~ ^(spa|confidential|off)$ ]] || die "--entra-web: spa, confidential or off"
[[ -z $acr || $storage_auth == identity ]] || die "--acr needs --storage-auth identity"
size=${size:-small}
panel=${panel:-true}
log_analytics=${log_analytics:-false}
telemetry=${telemetry:-false}
diagnostics=${diagnostics:-false}
headscale=${headscale:-false}
public_derp=${public_derp:-false}
[[ $headscale == false || $panel == true ]] || die "--headscale needs the panel (Headscale is served on https://<fqdn>): drop --no-panel"
[[ $headscale == false || -z $tailscale_secret_file ]] || die "--headscale and --tailscale-secret-file: one tailnet provider (the sidecar is the provider)"
! $log_analytics || ensure_providers Microsoft.OperationalInsights
# Application Insights on its Log Analytics workspace
! $telemetry || ensure_providers Microsoft.OperationalInsights Microsoft.Insights

if $build_in_acr; then
	# the image from this checkout, built by ACR Tasks (no docker here); a
	# registry in the hub's resource group, created on the first use
	ensure_providers Microsoft.ContainerRegistry
	root=$(cd "$here/../.." && pwd)
	[[ -f $root/deploy/hub/Dockerfile && -f $root/go.mod ]] || die "--build-in-acr: run from a checkout of the repository"
	build_acr=$(az acr list -g "$rg" --query "[?tags.\"longarm-hub\"=='$name'].name | [0]" -o tsv | tr -d '\r')
	if [[ -z $build_acr ]]; then
		build_acr=lahub$(od -An -tx1 -N6 /dev/urandom | tr -d ' \n')
		echo "creating the container registry $build_acr in $rg (Basic, about 5 USD/month)"
		# key: the hub pulls with the admin login; identity: with its managed identity (AcrPull)
		az acr create -g "$rg" -n "$build_acr" -l "$location" --sku Basic --admin-enabled "$([[ $storage_auth == key ]] && echo true || echo false)" \
			--tags "longarm-hub=$name" -o none
	fi
	login_server=$(az acr show -g "$rg" -n "$build_acr" --query loginServer -o tsv | tr -d '\r')
	version=$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)
	tag=build-$(date -u +%Y%m%d%H%M%S)
	echo "building $login_server/longarm-hub:$tag from $root (az acr build, a few minutes)"
	# --target acr: ACR Tasks can't parse the BuildKit stages (deploy/hub/Dockerfile)
	az acr build -g "$rg" -r "$build_acr" -t "longarm-hub:$tag" --target acr --platform linux/amd64 \
		--build-arg "VERSION=$version" -f deploy/hub/Dockerfile "$root" ||
		die "az acr build failed (the build log is above)"
	image=$login_server/longarm-hub:$tag
fi
# an image in an Azure Container Registry of this resource group: key: its
# admin login, identity: the managed identity pulls it (--acr); no
# --registry-user needed, also on re-runs
reg_host=$(registry_host "$image")
if [[ -z $registry_user && $reg_host == *.azurecr.io ]]; then
	reg_acr=${build_acr:-$(az acr list -g "$rg" --query "[?loginServer=='$reg_host'].name | [0]" -o tsv 2>/dev/null | tr -d '\r' || true)}
	if [[ $storage_auth == identity ]]; then
		acr=${acr:-$reg_acr}
	elif [[ -n $reg_acr ]]; then
		acr_login "$reg_acr"
	fi
fi
[[ -z $prev_registry || -n $registry_user || -n $acr ]] ||
	die "the hub pulls its image with a login for $prev_registry, which can't be read back: pass --registry-user ${prev_registry_user:-again} (password in LONGARM_REGISTRY_PASSWORD)"

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
# write_params FILE: the deployment's parameters from the settings above
write_params() {
	{
		printf '{"contentVersion":"1.0.0.0","parameters":{'
		printf '"name":{"value":%s}' "$(json_str "$name")"
		printf ',"size":{"value":%s}' "$(json_str "$size")"
		printf ',"enablePanel":{"value":%s}' "$panel"
		printf ',"enableLogAnalytics":{"value":%s}' "$log_analytics"
		[[ -z $gate_free_gb ]] || printf ',"schedulerGateFreeGb":{"value":%s}' "$(json_str "$gate_free_gb")"
		[[ -z $release_public_key ]] || printf ',"releasePublicKey":{"value":%s}' "$(json_str "$release_public_key")"
		printf ',"storageAuth":{"value":%s}' "$(json_str "$storage_auth")"
		printf ',"enableTelemetry":{"value":%s},"enableDiagnostics":{"value":%s}' "$telemetry" "$diagnostics"
		printf ',"enableHeadscale":{"value":%s},"headscalePublicDerp":{"value":%s}' "$headscale" "$public_derp"
		[[ -z $acr ]] || printf ',"acrName":{"value":%s}' "$(json_str "$acr")"
		[[ -z $image ]] || printf ',"image":{"value":%s}' "$(json_str "$image")"
		[[ -z $acme_email ]] || printf ',"acmeEmail":{"value":%s}' "$(json_str "$acme_email")"
		if [[ -n $entra_tenant ]]; then
			printf ',"entraTenantId":{"value":%s}' "$(json_str "$entra_tenant")"
			printf ',"entraClientId":{"value":%s}' "$(json_str "$entra_client")"
			[[ -z $entra_admins ]] || printf ',"entraAdmins":{"value":%s}' "$(json_str "$entra_admins")"
			[[ -z $entra_apps ]] || printf ',"entraApps":{"value":%s}' "$(json_str "$entra_apps")"
			[[ -z $entra_web ]] || printf ',"entraWeb":{"value":%s}' "$(json_str "$entra_web")"
		fi
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
	} >"$1"
}
write_params "$tmp/params.json"


# image_pullable IMAGE: the registry has the image's manifest, asked the way
# ACI pulls it (anonymously, or with the deployment's registry login; an
# Azure Container Registry pulled with the managed identity: az acr). The
# tests point LONGARM_DEPLOY_REGISTRY_URL at a fake registry.
image_problem=''
image_pullable() {
	local ref=$1 host repo reference rest last url code hdr=$tmp/registry.hdr ch realm service scope tok
	local accept='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
	host=$(registry_host "$ref")
	rest=${ref#"$host/"}
	if [[ -z $host || $host == docker.io ]]; then
		host=registry-1.docker.io
		rest=${ref#docker.io/}
		[[ $rest == */* ]] || rest=library/$rest
	fi
	if [[ $rest == *@* ]]; then
		repo=${rest%%@*}
		reference=${rest##*@}
		last=${repo##*/}
		[[ $last != *:* ]] || repo=${repo%:*}
	else
		last=${rest##*/}
		if [[ $last == *:* ]]; then
			repo=${rest%:*}
			reference=${rest##*:}
		else
			repo=$rest
			reference=latest
		fi
	fi
	if [[ $host == *.azurecr.io && $storage_auth == identity && -z $registry_user ]]; then
		if [[ $reference == sha256:* ]]; then
			az acr repository show -n "${host%%.*}" --image "$repo@$reference" -o none 2>/dev/null && return 0
		else
			az acr repository show -n "${host%%.*}" --image "$repo:$reference" -o none 2>/dev/null && return 0
		fi
		image_problem="az acr repository show -n ${host%%.*} --image $repo:$reference failed"
		return 1
	fi
	url=https://$host/v2/$repo/manifests/$reference
	[[ -z ${LONGARM_DEPLOY_REGISTRY_URL:-} ]] || url=$LONGARM_DEPLOY_REGISTRY_URL/$host/v2/$repo/manifests/$reference
	code=$(curl -sS -I -m 20 -o /dev/null -D "$hdr" -w '%{http_code}' -H "Accept: $accept" "$url" 2>/dev/null) || code=000
	if [[ $code == 401 ]]; then
		# a token for repository:<repo>:pull from the realm the registry names
		ch=$(tr -d '\r' <"$hdr" | sed -n 's/^[Ww][Ww][Ww]-[Aa]uthenticate: *[Bb]earer *//p' | head -n 1)
		realm=$(printf '%s' "$ch" | sed -n 's/.*realm="\([^"]*\)".*/\1/p')
		service=$(printf '%s' "$ch" | sed -n 's/.*service="\([^"]*\)".*/\1/p')
		scope=$(printf '%s' "$ch" | sed -n 's/.*scope="\([^"]*\)".*/\1/p')
		scope=${scope:-repository:$repo:pull}
		if [[ -n $realm ]]; then
			# the registry login (if the deployment has one for this registry) through curl's
			# config on stdin, not argv
			tok=$({
				if [[ -n $registry_user && $host == "$reg_host" ]]; then
					printf 'user = "%s:%s"\n' "$registry_user" "${LONGARM_REGISTRY_PASSWORD:-}"
				fi
			} | curl -sS -m 20 -K - "$realm?service=$service&scope=$scope" 2>/dev/null |
				sed -n 's/.*"\(access_\)\{0,1\}token" *: *"\([^"]*\)".*/\2/p' | head -n 1) || tok=''
			code=$(curl -sS -I -m 20 -o /dev/null -w '%{http_code}' -H "Accept: $accept" ${tok:+-H "Authorization: Bearer $tok"} "$url" 2>/dev/null) || code=000
		fi
	fi
	[[ $code == 200 ]] && return 0
	image_problem="the registry answered ${code/000/nothing} for $url"
	return 1
}

# check_images: every image of the deployment can be pulled
check_images() {
	local imgs img
	imgs=("${image:-$(template_default image)}")
	[[ $storage_auth != key || $panel != true ]] || imgs+=("$(template_default caddyImage)")
	[[ $headscale != true ]] || imgs+=("$(template_default headscaleImage)")
	for img in "${imgs[@]}"; do
		[[ -n $img ]] || continue
		image_pullable "$img" ||
			die "the image $img can't be pulled ($image_problem): nothing was changed. Check the tag, that the package is public (or --registry-user), or skip this check with --skip-image-check"
	done
}

echo "validating the deployment (nothing is changed yet)"
az deployment group validate -g "$rg" --template-file "$template" --parameters "@$tmp/params.json" -o none ||
	die "the deployment does not validate (the error is above, e.g. a policy violation): nothing was changed"
$skip_image_check || check_images

deleted=false
if $recreate && [[ -n $cur_image ]]; then
	# the previous configuration, for a rollback if the new one fails: the
	# settings swapped for the previous ones while its parameters are written
	swap='image:prev_image size:prev_size panel:prev_panel acr:prev_acr headscale:prev_headscale public_derp:prev_public_derp log_analytics:prev_logs telemetry:prev_telemetry diagnostics:prev_diagnostics stun_servers:prev_stun gate_free_gb:prev_gate release_public_key:prev_relkey entra_tenant:prev_entra_tenant entra_client:prev_entra_client entra_admins:prev_entra_admins entra_web:prev_entra_web entra_apps:prev_entra_apps'
	for kv in $swap; do
		v=${kv%%:*}
		printf -v "saved_$v" '%s' "${!v}"
	done
	for kv in $swap; do
		v=${kv#*:}
		printf -v "${kv%%:*}" '%s' "${!v}"
	done
	saved_reg_host=$reg_host saved_tailscale_secret_file=$tailscale_secret_file
	reg_host=$(registry_host "$image")
	[[ $prev_tailscale == true ]] || tailscale_secret_file=''
	write_params "$tmp/rollback.json"
	for kv in $swap; do
		v=saved_${kv%%:*}
		printf -v "${kv%%:*}" '%s' "${!v}"
	done
	reg_host=$saved_reg_host tailscale_secret_file=$saved_tailscale_secret_file
	az deployment group validate -g "$rg" --template-file "$template" --parameters "@$tmp/rollback.json" -o none ||
		die "the previous configuration (the rollback if the new one fails) does not validate (above): nothing was changed"
	echo "saving the hub's settings (resource group tags longarm-hub-prev-*: a re-run finds them if the group is gone)"
	snapshot_tags
	echo "deleting container group $name (storage and data stay)"
	az container delete -g "$rg" -n "$name" --yes -o none
	deleted=true
fi

# deploy_run FILE NAME: deploys and waits for the deployment; 1 = it
# failed ($deploy_err says why). --no-wait + polling: a container group
# stuck in Creating would block "az deployment group create" for an hour
# (check_stuck)
deploy_err=''
deploy_run() {
	local dstate='' end=$((SECONDS + timeout))
	stuck_since=''
	if ! az deployment group create -g "$rg" -n "$2" --template-file "$template" --parameters "@$1" --no-wait -o none; then
		deploy_err="az deployment group create $2 failed (above)"
		return 1
	fi
	until [[ $dstate == Succeeded ]]; do
		sleep "$poll"
		dstate=$(az deployment group show -g "$rg" -n "$2" --query properties.provisioningState -o tsv 2>/dev/null | tr -d '\r' || true)
		case $dstate in
		Failed | Canceled)
			az deployment operation group list -g "$rg" -n "$2" \
				--query "[?properties.provisioningState=='Failed'].{resource: properties.targetResource.resourceName, error: properties.statusMessage.error}" -o json >&2 || true
			deploy_err="deployment $2: $dstate (errors above; az deployment group show -g $rg -n $2)"
			return 1
			;;
		esac
		if ! check_stuck; then
			deploy_err="the container group $name is stuck in Creating (above)"
			return 1
		fi
		if ((SECONDS >= end)); then
			deploy_err="deployment $2 is still ${dstate:-not started} after ${timeout}s: az deployment group show -g $rg -n $2"
			return 1
		fi
	done
}

# fail_deploy MESSAGE: the new configuration failed. After --recreate
# deleted the group, the previous configuration is deployed again.
fail_deploy() {
	$deleted || die "$1"
	deleted=false
	echo "deploy.sh: $1" >&2
	echo "rolling back: deploying the previous configuration again (image ${prev_image:-the template default}, size $prev_size, panel $prev_panel, headscale $prev_headscale)" >&2
	az container delete -g "$rg" -n "$name" --yes -o none 2>/dev/null || true
	if deploy_run "$tmp/rollback.json" "longarm-hub-$(date -u +%Y%m%d%H%M%S)-rollback"; then
		local st='' end=$((SECONDS + timeout))
		until [[ $st == Running ]] || ((SECONDS >= end)); do
			sleep "$poll"
			st=$(az container show -g "$rg" -n "$name" --query "containers[?name=='hub'].instanceView.currentState.state" -o tsv 2>/dev/null | tr -d '\r' || true)
		done
		clear_snapshot
		die "$1
ROLLED BACK: the previous configuration is deployed again (hub ${st:-state unknown}; data kept). Nothing of the new configuration is running; fix the cause and run deploy.sh again."
	fi
	die "$1
The rollback failed too: $deploy_err
The hub's settings are saved in the resource group's tags (longarm-hub-prev-*): ./deploy.sh --name $name --resource-group $rg (without --recreate) deploys them again."
}

echo "deploying $name to $rg ($location, $size, panel $panel, storage auth $storage_auth$([[ $headscale == true ]] && echo ", headscale")${image:+, $image}); this takes a few minutes"
deadline=$((SECONDS + timeout))
logs_hint="az container logs -g $rg -n $name --container-name hub"
deployment=longarm-hub-$(date -u +%Y%m%d%H%M%S)
# phases of the redeploy (seconds since the submit; README "Redeploy downtime")
t_submit=$SECONDS
phase() { echo "  [+$((SECONDS - t_submit))s] $*"; }
deploy_run "$tmp/params.json" "$deployment" || fail_deploy "$deploy_err"
phase "deployment succeeded"
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
if [[ $storage_auth == key ]]; then
	key=$(az storage account keys list -g "$rg" -n "$account" --query "[0].value" -o tsv | tr -d '\r')
fi

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
	check_stuck || fail_deploy "the container group $name is stuck in Creating (above)"
	((SECONDS < deadline)) || fail_deploy "the hub container is not running ($state): $logs_hint"
	sleep "$poll"
done
phase "hub container running"
# bootstrap.env of this start (a new boot_id; restart once if the update
# didn't restart the hub)
restarted=false
until read_bootstrap && [[ $(bootstrap_value boot_id) != "$prev_boot" ]]; do
	((SECONDS < deadline)) || fail_deploy "no bootstrap values from this start: $logs_hint"
	if [[ -n $prev_boot ]] && ! $restarted && ((SECONDS > waited_from + 240)); then
		echo "the hub did not restart: az container restart"
		az container restart -g "$rg" -n "$name" -o none
		restarted=true
	fi
	sleep 2
done
phase "new hub start bootstrapped"
until tcp_open "$fqdn" "${hub_addr##*:}"; do
	((SECONDS < deadline)) || fail_deploy "$hub_addr does not answer: $logs_hint"
	sleep 2
done
phase "hub answers on $hub_addr"
# the new configuration runs: the settings snapshot has done its job
if $deleted || $have_snapshot; then
	clear_snapshot
fi
fp=$(bootstrap_value ca_fingerprint)
code=$(bootstrap_value admin_join_code)
expires=$(bootstrap_value admin_join_code_expires)
admin_joined=$(bootstrap_value admin_joined)

if [[ -n $panel_url ]]; then
	ok=false
	for _ in $(seq 1 18); do
		if curl -fsS -m 10 -o /dev/null "$panel_url/healthz" 2>/dev/null; then
			ok=true
			break
		fi
		sleep 10
	done
	$ok || echo "note: $panel_url/healthz does not answer yet (Let's Encrypt can take a few minutes): az container logs -g $rg -n $name --container-name $([[ $storage_auth == identity ]] && echo hub || echo caddy)" >&2
fi

# the tailnet: the hub's view of its Headscale sidecar (/healthz "headscale":
# ok | starting | down; the detail: longarm admin tailnet status)
tailnet_status='none (--headscale for a self-hosted one, or --tailscale-secret-file)'
if [[ $headscale == true ]]; then
	hs=''
	for _ in $(seq 1 18); do
		health=$(curl -fsS -m 10 "$panel_url/healthz" 2>/dev/null || true)
		hs=$(printf '%s' "$health" | sed -n 's/.*"headscale": *"\([a-z]*\)".*/\1/p')
		[[ $hs != ok ]] || break
		sleep "$poll"
	done
	tailnet_status="headscale $panel_url: ${hs:-unknown}"
	[[ $hs == ok ]] || echo "note: Headscale is ${hs:-not reported} after 3 min: az container logs -g $rg -n $name --container-name headscale (and --container-name hub); longarm admin tailnet status" >&2
elif [[ -n $tailscale_secret_file ]]; then
	tailnet_status='tailscale (OAuth client)'
fi

# Rough monthly cost (Linux ACI pay-as-you-go, about 0.0405 USD per vCPU-hour
# and 0.0045 USD per GB-hour in US regions; other regions differ by up to ~20 %).
cost() {
	awk -v s="$size" -v p="$panel" -v a="$storage_auth" -v h="$headscale" 'BEGIN {
		cpu = (s == "medium") ? 2 : 1; mem = (s == "medium") ? 3 : 1.5
		if (p == "true" && a == "key") { cpu += 0.25; mem += 0.3 } # Caddy
		if (h == "true") { cpu += 0.25; mem += 0.3 } # Headscale
		printf "%.0f", 730 * (cpu * 0.0405 + mem * 0.0045) + 1
	}'
}

cat <<EOF

longarm-hub is up.
  hub address     $hub_addr
  CA fingerprint  $fp
  panel           ${panel_url:-off (--no-panel)}
  tailnet         $tailnet_status
  resource group  $rg (storage account $account, storage auth $storage_auth)
  cost            about $(cost) USD/month (compute; plus egress over 100 GB/month, relay traffic counts)

EOF
if [[ -n $code ]]; then
	cat <<EOF
1. Your laptop (admin): install longarm (https://github.com/lubobill1990/longarm/releases), then
     longarm join $hub_addr $code --ca-fingerprint $fp
   The code is one-time and valid until $expires (a new one: ./deploy.sh --name $name --admin-code).
EOF
elif [[ $admin_joined == false ]]; then
	# storage-auth identity: the code is only in the container's bootstrap.env
	cat <<EOF
1. Your laptop (admin): install longarm (https://github.com/lubobill1990/longarm/releases), then
     ./deploy.sh --name $name --resource-group $rg --admin-code      # a one-time admin join code (24 h)
     longarm join $hub_addr <code> --ca-fingerprint $fp
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
