# shellcheck shell=bash disable=SC2154 # sourced by deploy.sh, which defines the variables and functions used here
# relay.sh: deploy.sh --relay / --relay-remove (sourced by deploy.sh, which
# has parsed the options and defined az, die, tcp_open, rg_exists,
# registry_host, acr_login, $tmp, $poll, $timeout). README "External relay".
#
# The hub's external longnet relay: its own container group <name>-relay
# (relay.json, compiled from relay.bicep) with its own public IP and DNS
# label, so a hub redeploy never restarts it. Its certificate comes from the
# hub's CA (az container exec ... longarm-hub-relay-cert, the existing
# `longarm-hub issue --role relay` path) and goes to the container as a
# secure environment variable; the relay registers with the hub, which
# advertises it to the nodes. No storage and no state of its own.

relay_template=$here/relay.json
relay_group=$name-relay
[[ -f $relay_template ]] || die "$relay_template is missing (run from a checkout of the repository)"
rg_exists || die "resource group $rg does not exist: deploy the hub first (./deploy.sh --name $name --location <region>)"

if $relay_remove; then
	if ! $yes; then
		echo "This deletes the external relay $relay_group in $rg (the hub keeps its embedded relay)."
		read -r -p "Type the hub name ($name) to continue: " answer
		[[ $answer == "$name" ]] || die "aborted"
	fi
	az container delete -g "$rg" -n "$relay_group" --yes -o none
	echo "deleted $relay_group. The hub drops it from the relay set 7 days after it last saw it; at once with: longarm admin revoke $relay_group"
	exit 0
fi

[[ -n $location ]] || location=$(az group show -n "$rg" --query location -o tsv | tr -d '\r')
ensure_providers Microsoft.ContainerInstance

# The hub: its image (the relay runs the same one), its registry login, its
# STUN servers, and that it runs (it issues the certificate and is what the
# relay registers with).
hub_info=$(az container show -g "$rg" -n "$name" -o tsv \
	--query "[containers[?name=='hub'].image | [0], ipAddress.fqdn, imageRegistryCredentials[0].server, imageRegistryCredentials[0].identity, containers[?name=='hub'].instanceView.currentState.state | [0], containers[?name=='hub'].environmentVariables[] | [?name=='LONGARM_HUB_RELAY_STUN_ADVERTISE'].value | [0]]" \
	2>/dev/null || true)
{
	read -r hub_image
	read -r hub_fqdn
	read -r hub_reg
	read -r hub_reg_identity
	read -r hub_state
	read -r hub_stun || true
} <<<"$(printf '%s\n' "$hub_info" | tr -d '\r' | tr '\t' '\n' | sed 's/^None$//')"
[[ -n $hub_image && -n $hub_fqdn ]] || die "no hub container group $name in $rg: deploy the hub first"
[[ $hub_state == Running ]] || die "the hub is ${hub_state:-not running}: the relay gets its certificate from the hub and registers with it - start the hub first (az container start -g $rg -n $name)"
image=${image:-$hub_image}
reg_host=$(registry_host "$image")

# the registry: the hub's managed identity (an ACR in identity mode), a
# login, or anonymous
relay_acr=''
if [[ -n $hub_reg_identity && $reg_host == "$hub_reg" ]]; then
	relay_acr=${hub_reg%%.*}
elif [[ -z $registry_user && $reg_host == *.azurecr.io ]]; then
	reg_acr=$(az acr list -g "$rg" --query "[?loginServer=='$reg_host'].name | [0]" -o tsv 2>/dev/null | tr -d '\r' || true)
	[[ -z $reg_acr ]] || acr_login "$reg_acr"
fi
[[ -n $relay_acr || -n $registry_user || -z $hub_reg || $reg_host != "$hub_reg" ]] ||
	die "the hub pulls its image with a login for $hub_reg, which can't be read back: pass --registry-user (password in LONGARM_REGISTRY_PASSWORD)"
[[ -z $registry_user || -n ${LONGARM_REGISTRY_PASSWORD:-} ]] || die "--registry-user needs LONGARM_REGISTRY_PASSWORD"

# STUN: the hub's (the template sets self[,<servers>]) unless given
if [[ -z $stun_servers ]]; then
	case $hub_stun in
	self) stun_servers=none ;;
	self,*) stun_servers=${hub_stun#self,} ;;
	esac
fi

# the previous relay (rollback): its image
prev_relay_image=$(az container show -g "$rg" -n "$relay_group" --query "containers[0].image" -o tsv 2>/dev/null | tr -d '\r' | sed 's/^None$//' || true)

# The certificate, issued by the hub from its CA: base64 of a tar.gz with
# ca.crt, node.crt, node.key (role relay, name <name>-relay, valid a year;
# the relay renews it through the hub). It is a private key: only in $tmp
# and the deployment's secure parameter.
echo "issuing the relay certificate from the hub's CA (az container exec $name longarm-hub-relay-cert)"
# MSYS_NO_PATHCONV: Git Bash on Windows would turn the path into C:/Program Files/Git/usr/...
MSYS_NO_PATHCONV=1 az container exec -g "$rg" -n "$name" --container-name hub \
	--exec-command /usr/local/bin/longarm-hub-relay-cert >"$tmp/relay-cert.out" 2>/dev/null || true
relay_tls=$(tr -d '\r' <"$tmp/relay-cert.out" | grep -E '^[A-Za-z0-9+/=]{200,}$' | tail -n 1 || true)
rm -f "$tmp/relay-cert.out"
[[ -n $relay_tls ]] ||
	die "no relay certificate from the hub (az container exec ... /usr/local/bin/longarm-hub-relay-cert): its image may predate external relays - upgrade the hub first (./deploy.sh --name $name --image <image:tag>)"
files=$(printf '%s' "$relay_tls" | base64 --decode 2>/dev/null | tar -tzf - 2>/dev/null | sort | tr '\n' ' ' || true)
[[ $files == "ca.crt node.crt node.key " ]] || die "the hub's relay certificate bundle is not ca.crt, node.crt, node.key (got: ${files:-unreadable})"

# relay_params FILE IMAGE: the relay deployment's parameters (secrets in the file only)
relay_params() {
	{
		printf '{"contentVersion":"1.0.0.0","parameters":{'
		printf '"name":{"value":%s}' "$(json_str "$name")"
		printf ',"image":{"value":%s}' "$(json_str "$2")"
		printf ',"hubAddress":{"value":%s}' "$(json_str "$hub_fqdn:7443")"
		printf ',"relayTls":{"value":%s}' "$(json_str "$relay_tls")"
		[[ -z $stun_servers ]] || printf ',"stunServers":{"value":%s}' "$(json_str "$([[ $stun_servers == none ]] || printf %s "$stun_servers")")"
		[[ -z $relay_acr ]] || printf ',"acrName":{"value":%s}' "$(json_str "$relay_acr")"
		if [[ -n $registry_user && -z $relay_acr ]]; then
			[[ -z $reg_host ]] || printf ',"registryServer":{"value":%s}' "$(json_str "$reg_host")"
			printf ',"registryUsername":{"value":%s}' "$(json_str "$registry_user")"
			printf ',"registryPassword":{"value":%s}' "$(json_str "$LONGARM_REGISTRY_PASSWORD")"
		fi
		printf '}}\n'
	} >"$1"
}
json_str() {
	local s=$1
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	s=${s//$'\n'/\\n}
	s=${s//$'\r'/}
	printf '"%s"' "$s"
}
relay_params "$tmp/relay.json" "$image"
echo "validating the relay deployment (nothing is changed yet)"
az deployment group validate -g "$rg" --template-file "$relay_template" --parameters "@$tmp/relay.json" -o none ||
	die "the relay deployment does not validate (the error is above, e.g. a policy violation): nothing was changed"
if [[ -n $prev_relay_image ]]; then
	relay_params "$tmp/relay-rollback.json" "$prev_relay_image"
	az deployment group validate -g "$rg" --template-file "$relay_template" --parameters "@$tmp/relay-rollback.json" -o none ||
		die "the previous relay configuration (the rollback) does not validate (above): nothing was changed"
fi

# relay_run FILE NAME: deploy and wait for the deployment (1 = failed: $relay_err)
relay_err=''
relay_run() {
	local dstate='' end=$((SECONDS + timeout))
	if ! az deployment group create -g "$rg" -n "$2" --template-file "$relay_template" --parameters "@$1" --no-wait -o none; then
		relay_err="az deployment group create $2 failed (above)"
		return 1
	fi
	until [[ $dstate == Succeeded ]]; do
		sleep "$poll"
		dstate=$(az deployment group show -g "$rg" -n "$2" --query properties.provisioningState -o tsv 2>/dev/null | tr -d '\r' || true)
		case $dstate in
		Failed | Canceled)
			az deployment operation group list -g "$rg" -n "$2" \
				--query "[?properties.provisioningState=='Failed'].{resource: properties.targetResource.resourceName, error: properties.statusMessage.error}" -o json >&2 || true
			relay_err="deployment $2: $dstate (errors above)"
			return 1
			;;
		esac
		if ((SECONDS >= end)); then
			relay_err="deployment $2 is still ${dstate:-not started} after ${timeout}s"
			return 1
		fi
	done
}

# relay_up DEPLOYMENT: the relay container runs and has registered with the
# hub (its log line), and its TLS fallback answers (1 = not within --timeout)
relay_fqdn='' relay_tcp=''
relay_up() {
	local st='' end=$((SECONDS + timeout)) out
	until [[ $st == Running ]]; do
		st=$(az container show -g "$rg" -n "$relay_group" --query "containers[0].instanceView.currentState.state" -o tsv 2>/dev/null | tr -d '\r' || true)
		[[ $st == Running ]] && break
		((SECONDS < end)) || { relay_err="the relay container is ${st:-not running}"; return 1; }
		sleep "$poll"
	done
	out=$(az deployment group show -g "$rg" -n "$1" -o tsv --query "[properties.outputs.fqdn.value, properties.outputs.relayTcp.value]" 2>/dev/null | tr -d '\r' | tr '\t' '\n')
	{
		read -r relay_fqdn
		read -r relay_tcp || true
	} <<<"$out"
	[[ -n $relay_fqdn && $relay_tcp == *:* ]] || { relay_err="deployment $1 returned no outputs"; return 1; }
	until az container logs -g "$rg" -n "$relay_group" --container-name relay 2>/dev/null | grep -q 'relay: subscribed to the hub'; do
		((SECONDS < end)) || { relay_err="the relay did not register with the hub (az container logs -g $rg -n $relay_group)"; return 1; }
		sleep "$poll"
	done
	until tcp_open "${relay_tcp%:*}" "${relay_tcp##*:}"; do
		((SECONDS < end)) || { relay_err="$relay_tcp does not answer"; return 1; }
		sleep 5
	done
}

# relay_fail MESSAGE: the new relay failed; the previous one (if any) is
# deployed again. The hub is never touched: its embedded relay works throughout.
relay_fail() {
	[[ -n $prev_relay_image ]] || die "$1
The hub is unaffected (its embedded relay keeps working). Fix the cause and run ./deploy.sh --name $name --relay again, or remove the relay: ./deploy.sh --name $name --relay-remove"
	echo "deploy.sh: $1" >&2
	echo "rolling back: deploying the previous relay again (image $prev_relay_image)" >&2
	local dep
	dep=longarm-relay-$(date -u +%Y%m%d%H%M%S)-rollback
	if relay_run "$tmp/relay-rollback.json" "$dep" && relay_up "$dep"; then
		die "$1
ROLLED BACK: the previous relay ($prev_relay_image) runs again. The hub is unaffected."
	fi
	die "$1
The rollback failed too: $relay_err
The hub is unaffected (its embedded relay keeps working)."
}

echo "deploying the external relay $relay_group to $rg ($location, $image); a few minutes"
relay_dep=longarm-relay-$(date -u +%Y%m%d%H%M%S)
relay_run "$tmp/relay.json" "$relay_dep" || relay_fail "$relay_err"
relay_up "$relay_dep" || relay_fail "$relay_err"

cat <<EOF

The external relay is up.
  relay           $relay_fqdn  udp 7445 (QUIC + STUN), 7446 (STUN), tcp 7443 (TLS fallback)
  registered with $hub_fqdn:7443 (longarm admin relays shows it and how many workers use it)
  cost            about 16 USD/month (0.5 vCPU / 0.5 GB; plus egress over 100 GB/month: relayed traffic)

Workers that know the relay set (this release and later) listen on it within
seconds; clients reach them through it from their next command. Older
workers keep using the hub's embedded relay until they update (longarm
worker update, the tray's update banner, or self-update). Nothing needs
signing in again. Hub redeploys no longer drop connections that go through
this relay.

Upgrade the relay with the hub's image: ./deploy.sh --name $name --relay
Remove it:                              ./deploy.sh --name $name --relay-remove
EOF
exit 0
