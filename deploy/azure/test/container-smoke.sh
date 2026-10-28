#!/usr/bin/env bash
# container-smoke.sh <image> <longarm binary> [<tsprobe binary>]
# The longarm-hub image as deploy/azure runs it, with Azurite standing in for
# Blob Storage and a docker volume for the Azure Files share (CI:
# .github/workflows/image.yml; needs docker and the Azure CLI):
#   1. first start: restore (nothing), bootstrap (CA, hub.toml, admin code),
#      health, an admin joins with the printed code, Litestream replicates
#   2. container replaced (docker stop = SIGTERM, rm; the local database is
#      gone): the new one restores it from the replica - writes made just
#      before the stop are there, the CA and the admin's certificate still work
#   3. storage-auth identity (no share; the data dir dies with the container):
#      a published release is still served after the container is replaced
#      (the files in [releases] blob_url - a docker volume as file:// here,
#      as Azurite has no Entra tokens - and the CA in the database)
#   4. the Headscale sidecar (main.bicep enableHeadscale: its image and start
#      script from azuredeploy.json): the hub restores its database, writes
#      its config and keys, creates its API key over the socket, proxies
#      /key and /ts2021; a tsnet node (tsprobe) registers through the hub
#      with a key the hub minted; after both containers and the shared
#      volume are replaced, Headscale's nodes, Noise key and API key survive
#   5. a replica that can't be read stops the start
set -euo pipefail

image=${1:?usage: container-smoke.sh <image> <longarm binary> [<tsprobe binary>]}
longarm=${2:?usage: container-smoke.sh <image> <longarm binary> [<tsprobe binary>]}
tsprobe=${3:-}
template=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/azuredeploy.json
azurite_image=mcr.microsoft.com/azure-storage/azurite:3.37.0@sha256:830430c1da1a2d537e08f3e6764dd1f5ae00cf0346bcaf625b968ec3f0971fd5
p=lasmoke$$
# Azurite's well-known development account (public, documented by Microsoft)
acct=devstoreaccount1
key=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==
conn="DefaultEndpointsProtocol=http;AccountName=$acct;AccountKey=$key;BlobEndpoint=http://127.0.0.1:10000/$acct;"
work=$(mktemp -d)

cleanup() {
	rc=$?
	if [[ $rc -ne 0 ]]; then
		for c in "$p-hub" "$p-hub2" "$p-hub3" "$p-id" "$p-id2" "$p-hsh" "$p-hs" "$p-hsh2" "$p-hs2"; do
			if docker inspect "$c" >/dev/null 2>&1; then
				echo "::group::docker logs $c"
				docker logs "$c" 2>&1 | tail -n 200
				echo "::endgroup::"
			fi
		done
	fi
	docker rm -f "$p-hub" "$p-hub2" "$p-hub3" "$p-id" "$p-id2" "$p-https" "$p-hs" "$p-hs2" "$p-hsh" "$p-hsh2" "$p-azurite" >/dev/null 2>&1 || true
	docker volume rm "$p-share" "$p-rel" "$p-hsshare" "$p-hsvol" "$p-hsvol2" >/dev/null 2>&1 || true
	docker network rm "$p" >/dev/null 2>&1 || true
	rm -rf "$work"
	exit "$rc"
}
trap cleanup EXIT

step() { echo "== $*"; }
fail() {
	echo "::error::container-smoke: $*" >&2
	exit 1
}

docker network create "$p" >/dev/null
docker volume create "$p-share" >/dev/null
docker run -d --name "$p-azurite" --network "$p" --network-alias azurite -p 127.0.0.1:10000:10000 \
	"$azurite_image" azurite-blob --blobHost 0.0.0.0 --skipApiVersionCheck --loose >/dev/null
for _ in $(seq 1 30); do
	az storage container create -n litestream --connection-string "$conn" -o none 2>/dev/null && break
	sleep 1
done
[[ $(az storage container exists -n litestream --connection-string "$conn" -o tsv) == True ]] || fail "azurite: no container"
# Litestream gets a SAS for its container only, like on Azure (main.bicep)
sas=$(az storage container generate-sas -n litestream --connection-string "$conn" --permissions racwdl \
	--expiry "$(date -u -d '+1 day' +%Y-%m-%dT%H:%MZ)" -o tsv)
[[ -n $sas ]] || fail "no SAS"
hub_sas=$sas
hub_mount=(-v "$p-share:/data")
hub_replica=litestream

# hub_run NAME [extra docker args]: the environment of deploy/azure/main.bicep
hub_run() {
	local name=$1
	shift
	docker run -d --name "$name" --network "$p" \
		-p 127.0.0.1:7443:7443/tcp -p 127.0.0.1:7443:7443/udp -p 127.0.0.1:7444:7444/udp \
		"${hub_mount[@]}" \
		-e LONGARM_HUB_DATA_DIR=/data \
		-e LONGARM_HUB_CONFIG=/data/hub.toml \
		-e LONGARM_HUB_DB=/var/lib/longarm-hub/hub.db \
		-e LONGARM_HUB_ADVERTISE=127.0.0.1 \
		-e LONGARM_HUB_HTTP=127.0.0.1:7480 \
		-e LONGARM_HUB_BOOTSTRAP=1 \
		-e LONGARM_HUB_BOOTSTRAP_TTL=24h \
		-e LONGARM_HUB_RELAY_UDP=:7443 \
		-e LONGARM_HUB_RELAY_STUN2=:7444 \
		-e LONGARM_HUB_LITESTREAM_CONFIG=/etc/longarm-hub/litestream-abs.yml \
		-e LONGARM_HUB_BLOB_ACCOUNT=$acct \
		-e LONGARM_HUB_BLOB_CONTAINER=$hub_replica \
		-e LONGARM_HUB_BLOB_ENDPOINT=http://azurite:10000/$acct \
		-e LITESTREAM_AZURE_SAS_TOKEN="$hub_sas" \
		"$@" "$image" >/dev/null
}

wait_healthy() {
	local name=$1
	for _ in $(seq 1 60); do
		if [[ $(docker inspect -f '{{.State.Running}}' "$name") != true ]]; then
			fail "$name exited: $(docker inspect -f '{{.State.ExitCode}}' "$name")"
		fi
		docker exec "$name" /usr/local/bin/longarm-hub healthcheck >/dev/null 2>&1 && return 0
		sleep 1
	done
	fail "$name not healthy in 60 s"
}

# bootstrap NAME KEY: a value of the LONGARM_BOOTSTRAP log line
bootstrap() {
	docker logs "$1" 2>/dev/null | sed -n 's/^LONGARM_BOOTSTRAP //p' | tail -n 1 | tr ' ' '\n' | sed -n "s/^$2=//p"
}

# bootfile NAME KEY: a value of bootstrap.env on the share (the only place
# with the admin code)
bootfile() {
	docker cp "$1:/data/bootstrap.env" "$work/bootstrap.env" >/dev/null
	sed -n "s/^$2=//p" "$work/bootstrap.env"
}

# logs_have NAME TEXT: the container log contains TEXT (not "docker logs | grep -q":
# grep's early exit makes docker logs fail with SIGPIPE, and pipefail reports that)
logs_have() {
	local l
	l=$(docker logs "$1" 2>&1)
	grep -qF -- "$2" <<<"$l"
}

la() { LONGARM_CONFIG_DIR=$work/laptop "$longarm" "$@"; }

step "first start"
hub_run "$p-hub"
wait_healthy "$p-hub"
fp=$(bootstrap "$p-hub" ca_fingerprint)
addr=$(bootstrap "$p-hub" hub_address)
boot1=$(bootstrap "$p-hub" boot_id)
code=$(bootfile "$p-hub" admin_join_code)
[[ ${#fp} -eq 64 && -n $code && $addr == 127.0.0.1:7443 ]] || fail "bootstrap: fp=$fp code=$code addr=$addr"
[[ $(bootfile "$p-hub" ca_fingerprint) == "$fp" && $(bootfile "$p-hub" boot_id) == "$(bootstrap "$p-hub" boot_id)" ]] ||
	fail "bootstrap.env and the log line differ"
logs_have "$p-hub" "$code" && fail "the admin join code is in the container log"
[[ $(bootstrap "$p-hub" admin_joined) == false ]] || fail "admin_joined on a fresh hub"
logs_have "$p-hub" "no matching backups found" || fail "first start: no empty-replica restore"
[[ $(docker exec "$p-hub" /usr/local/bin/longarm-hub ca show) == *"$fp"* ]] || fail "ca show"
# runs as nonroot (65532), the share holds the state, not the database
[[ $(docker exec "$p-hub" /usr/local/bin/longarm-hub --version) == *longarm-hub* ]] || fail "version"
[[ $(docker inspect -f '{{.Config.User}}' "$p-hub") == nonroot:nonroot ]] || fail "image user"
docker run --rm --entrypoint /usr/local/bin/longarm-hub "$image" healthcheck --url http://127.0.0.1:1/ >/dev/null 2>&1 &&
	fail "healthcheck against nothing succeeded"

step "admin joins with the printed code"
la join "$addr" "$code" --ca-fingerprint "$fp" --name ci-admin
la admin join-code --role worker >/dev/null
docker exec -i "$p-hub" /usr/local/bin/longarm-hub user add alice --password-stdin <<<'a long passphrase' >/dev/null
# a write right before the stop: must survive through the shutdown sync
# the argument-free aliases for az container exec
[[ $(docker exec "$p-hub" /usr/local/bin/longarm-hub-admin-code 2>/dev/null) =~ ^[0-9A-Z]{3}-[0-9A-Z]{3}$ ]] || fail "longarm-hub-admin-code"
[[ $(docker exec "$p-hub" /usr/local/bin/longarm-hub-users) == *alice* ]] || fail "longarm-hub-users"
[[ $(docker exec "$p-hub" /usr/local/bin/longarm-hub-release-cert 2>/dev/null | base64 -d | tar -tz | sort | tr '\n' ' ') == "ca.crt node.crt node.key " ]] ||
	fail "longarm-hub-release-cert"
docker exec -i "$p-hub" /usr/local/bin/longarm-hub user add bob --password-stdin <<<'another passphrase' >/dev/null

step "replace the container (SIGTERM, rm)"
docker stop -t 60 "$p-hub" >/dev/null
logs_have "$p-hub" "longarm-hub stopped" || fail "hub did not stop gracefully"
logs_have "$p-hub" "litestream shut down" || fail "litestream did not shut down"
[[ $(docker inspect -f '{{.State.ExitCode}}' "$p-hub") == 0 ]] || fail "exit code $(docker inspect -f '{{.State.ExitCode}}' "$p-hub")"
docker rm "$p-hub" >/dev/null
n=$(az storage blob list -c litestream --connection-string "$conn" --query "length(@)" -o tsv)
[[ $n -gt 0 ]] || fail "no replica in blob storage"

hub_run "$p-hub2"
wait_healthy "$p-hub2"
logs_have "$p-hub2" "no matching backups found" && fail "hub2 found no replica"
[[ $(bootstrap "$p-hub2" ca_fingerprint) == "$fp" ]] || fail "CA changed"
[[ $(bootstrap "$p-hub2" admin_joined) == true ]] || fail "the restored database lost the admin"
[[ -z $(bootfile "$p-hub2" admin_join_code) ]] || fail "a new admin code after the admin joined"
[[ $(bootstrap "$p-hub2" boot_id) != "$boot1" ]] || fail "same boot_id after the replacement"
users=$(docker exec "$p-hub2" /usr/local/bin/longarm-hub user ls)
grep -qx alice <<<"$users" || fail "alice lost: $users"
grep -qx bob <<<"$users" || fail "bob (written just before the stop) lost: $users"
la admin join-code --role worker >/dev/null || fail "admin certificate after the replacement"
docker rm -f "$p-hub2" >/dev/null

step "storage-auth identity: a release survives replacing the container"
# its own replica (this hub keeps its CA in the database) and no share; the
# release container is a volume owned by nonroot (Azurite's image has chown)
az storage container create -n litestream-id --connection-string "$conn" -o none
hub_sas=$(az storage container generate-sas -n litestream-id --connection-string "$conn" --permissions racwdl \
	--expiry "$(date -u -d '+1 day' +%Y-%m-%dT%H:%MZ)" -o tsv)
docker volume create "$p-rel" >/dev/null
docker run --rm -v "$p-rel:/r" --entrypoint chown "$azurite_image" 65532:65532 /r
hub_mount=(-v "$p-rel:/releases")
hub_replica=litestream-id
id_run() {
	hub_run "$1" -p 127.0.0.1:7480:7480 -e LONGARM_HUB_HTTP=:7480 -e LONGARM_HUB_CA_IN_DB=1 \
		-e LONGARM_HUB_RELEASES_BLOB_URL=file:///releases
}
id_run "$p-id"
wait_healthy "$p-id"
idfp=$(bootstrap "$p-id" ca_fingerprint)
idcode=$(bootfile "$p-id" admin_join_code)
[[ ${#idfp} -eq 64 && $idfp != "$fp" && -n $idcode ]] || fail "identity hub bootstrap: fp=$idfp"
lai() { LONGARM_CONFIG_DIR=$work/laptop-id "$longarm" "$@"; }
lai join 127.0.0.1:7443 "$idcode" --ca-fingerprint "$idfp" --name ci-admin
mkdir -p "$work/rel"
printf 'longarmd 0.0.1\n' >"$work/rel/longarmd-linux-amd64"
head -c 300000 /dev/urandom >"$work/rel/longarm-windows-amd64.msi"
lai admin release publish "$work/rel" --version 0.0.1 --no-sign >/dev/null
curl -fsS -o "$work/got.msi" http://127.0.0.1:7480/releases/0.0.1/longarm-windows-amd64.msi || fail "download before the replacement"
cmp -s "$work/got.msi" "$work/rel/longarm-windows-amd64.msi" || fail "downloaded msi differs"
docker stop -t 60 "$p-id" >/dev/null
docker rm "$p-id" >/dev/null
id_run "$p-id2"
wait_healthy "$p-id2"
[[ $(bootstrap "$p-id2" ca_fingerprint) == "$idfp" ]] || fail "identity hub: CA changed"
rm -f "$work/got.msi"
curl -fsS -o "$work/got.msi" http://127.0.0.1:7480/releases/0.0.1/longarm-windows-amd64.msi ||
	fail "the release file was lost with the container"
cmp -s "$work/got.msi" "$work/rel/longarm-windows-amd64.msi" || fail "msi from the release container differs"
script=$(curl -fsS http://127.0.0.1:7480/install.sh) || fail "install.sh"
grep -qF "VERSION='0.0.1'" <<<"$script" || fail "install.sh doesn't hand out 0.0.1"
logs_have "$p-id2" "RELEASE FILES MISSING" && fail "the replaced hub reports missing release files"
docker rm -f "$p-id2" >/dev/null
hub_mount=(-v "$p-share:/data")
hub_replica=litestream
hub_sas=$sas

step "headscale sidecar"
# the template's image and start script (variables.headscaleScript)
hs_image=$(jq -r '.parameters.headscaleImage.defaultValue' "$template")
hs_script=$(jq -r '.variables.headscaleScript' "$template")
[[ $hs_image == *@sha256:* && $hs_script == *ready* ]] || fail "headscale image / script not in $template"
az storage container create -n litestream-hs --connection-string "$conn" -o none
az storage container create -n headscale --connection-string "$conn" -o none
sas_for() {
	az storage container generate-sas -n "$1" --connection-string "$conn" --permissions racwdl \
		--expiry "$(date -u -d '+1 day' +%Y-%m-%dT%H:%MZ)" -o tsv
}
hub_sas=$(sas_for litestream-hs)
hs_sas=$(sas_for headscale)
docker volume create "$p-hsshare" >/dev/null
hub_mount=(-v "$p-hsshare:/data")
hub_replica=litestream-hs
# hs_run HUB SIDECAR VOLUME: the hub (with the panel on 7480, the template's
# environment) and the sidecar in its network namespace, sharing VOLUME
hs_run() {
	hub_run "$1" -p 127.0.0.1:7480:7480 -e LONGARM_HUB_HTTP=:7480 -v "$3:/headscale" \
		-e LONGARM_HUB_LITESTREAM_CONFIG=/etc/longarm-hub/litestream-abs-headscale.yml \
		-e LONGARM_HUB_TAILNET_PROVIDER=headscale \
		-e LONGARM_HUB_HEADSCALE_URL=http://127.0.0.1:7480 \
		-e LONGARM_HUB_HEADSCALE_API_URL=http://127.0.0.1:8080 \
		-e LONGARM_HUB_HEADSCALE_API_KEY_FILE=/var/lib/longarm-hub/headscale-api.key \
		-e LONGARM_HUB_HEADSCALE_DIR=/headscale \
		-e LONGARM_HUB_HEADSCALE_BLOB_CONTAINER=headscale \
		-e LONGARM_HUB_HEADSCALE_SAS_TOKEN="$hs_sas"
	docker run -d --name "$2" --network "container:$1" -v "$3:/headscale" \
		--entrypoint /busybox/sh "$hs_image" -c "$hs_script" >/dev/null
}
hs_ok() {
	local h=''
	for _ in $(seq 1 90); do
		h=$(curl -fsS -m 5 http://127.0.0.1:7480/healthz 2>/dev/null || true)
		[[ $h != *'"headscale":"ok"'* ]] || return 0
		sleep 1
	done
	fail "headscale not ok via the hub: $h"
}
hs() { docker exec "$1" /ko-app/headscale --config /headscale/config.yaml "${@:2}"; }
docker volume create "$p-hsvol" >/dev/null
hs_run "$p-hsh" "$p-hs" "$p-hsvol"
wait_healthy "$p-hsh"
hs_ok
logs_have "$p-hsh" "headscale: new API key" || fail "the hub created no API key"
# no secret in the logs or the environment
hskey=$(docker cp "$p-hsh:/var/lib/longarm-hub/headscale-api.key" - | tar -xO)
[[ $hskey == hskey-api-* ]] || fail "api key file"
logs_have "$p-hsh" "$hskey" && fail "the API key is in the hub's log"
logs_have "$p-hs" "$hskey" && fail "the API key is in headscale's log"
[[ $(hs "$p-hs" apikeys list -o json | jq length) == 1 ]] || fail "not one API key: $(hs "$p-hs" apikeys list)"
# the proxy: Headscale's /key through the hub, its API not
key1=$(curl -fsS 'http://127.0.0.1:7480/key?v=130')
[[ $key1 == *'"publicKey":"mkey:'* ]] || fail "/key through the hub: $key1"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $hskey" http://127.0.0.1:7480/api/v1/apikey)
[[ $code != 200 && $code != 401 ]] || fail "Headscale's API is reachable through the hub ($code)"
derp=$(curl -s -o /dev/null -w '%{http_code}' --http1.1 -m 5 -H 'Connection: Upgrade' -H 'Upgrade: DERP' http://127.0.0.1:7480/derp || true)
[[ $derp == 101 ]] || fail "DERP upgrade through the hub: $derp"
fph=$(bootstrap "$p-hsh" ca_fingerprint)
hcode=$(bootfile "$p-hsh" admin_join_code)
lah() { LONGARM_CONFIG_DIR=$work/laptop-hs "$longarm" "$@"; }
lah join 127.0.0.1:7443 "$hcode" --ca-fingerprint "$fph" --name ci-admin
st=$(lah admin tailnet status)
[[ $st == *"provider  headscale"* && $st == *"health    ok"* ]] || fail "tailnet status: $st"
if [[ -n $tsprobe ]]; then
	# a node registers through the hub's proxy (/key, /ts2021) with a key the hub minted
	"$tsprobe" -config-dir "$work/laptop-hs" -hub 127.0.0.1:7443 || fail "tsprobe"
	nodes=$(hs "$p-hs" nodes list -o json)
	[[ $(jq length <<<"$nodes") -ge 1 && $nodes == *tag:longarm-client* ]] || fail "headscale nodes: $nodes"
fi

step "headscale sidecar: both containers and the shared volume replaced"
docker stop -t 60 "$p-hsh" >/dev/null
docker rm -f "$p-hs" >/dev/null
docker rm "$p-hsh" >/dev/null
[[ $(az storage blob list -c headscale --connection-string "$conn" --query "length(@)" -o tsv) -gt 0 ]] ||
	fail "no Headscale replica in blob storage"
docker volume create "$p-hsvol2" >/dev/null
hs_run "$p-hsh2" "$p-hs2" "$p-hsvol2"
wait_healthy "$p-hsh2"
hs_ok
logs_have "$p-hsh2" "headscale: new API key" && fail "a new API key after the replacement"
logs_have "$p-hsh2" "headscale: new private key" && fail "new private keys after the replacement"
[[ $(curl -fsS 'http://127.0.0.1:7480/key?v=130') == "$key1" ]] || fail "Headscale's Noise key changed"
[[ $(hs "$p-hs2" apikeys list -o json | jq length) == 1 ]] || fail "API keys after the replacement: $(hs "$p-hs2" apikeys list)"
if [[ -n $tsprobe ]]; then
	nodes2=$(hs "$p-hs2" nodes list -o json)
	[[ $nodes2 == *tag:longarm-client* ]] || fail "Headscale lost its nodes: $nodes2"
	"$tsprobe" -config-dir "$work/laptop-hs" -hub 127.0.0.1:7443 || fail "tsprobe after the replacement"
fi
docker rm -f "$p-hsh2" "$p-hs2" >/dev/null
hub_mount=(-v "$p-share:/data")
hub_replica=litestream
hub_sas=$sas

step "https on :443 as nonroot (file capability)"
docker run -d --name "$p-https" -e LONGARM_HUB_BOOTSTRAP=1 -e LONGARM_HUB_CONFIG=/data/hub.toml 	-e LONGARM_HUB_RELAY_UDP=off -e LONGARM_HUB_PUBLIC_URL=https://hub.example.com 	-e LONGARM_HUB_HTTPS=:443 -e LONGARM_HUB_HTTPS_REDIRECT=:80 "$image" >/dev/null
for _ in $(seq 1 30); do
	logs_have "$p-https" "longarm-hub started" && break
	sleep 1
done
logs_have "$p-https" 'https=[::]:443' || fail "https did not listen on :443: $(docker logs "$p-https" 2>&1 | tail -n 5)"
docker rm -f "$p-https" >/dev/null

step "unreadable replica: no start"
hub_sas="sv=2021-08-06&sr=c&sp=racwdl&se=2030-01-01T00:00Z&sig=bm90LWEtc2lnbmF0dXJl"
hub_run "$p-hub3" --restart no
for _ in $(seq 1 60); do
	[[ $(docker inspect -f '{{.State.Running}}' "$p-hub3") == true ]] || break
	sleep 1
done
[[ $(docker inspect -f '{{.State.Running}}' "$p-hub3") == false ]] || fail "started with a wrong storage key"
logs_have "$p-hub3" "litestream restore" || fail "no restore error in the log"

echo "container-smoke: ok"
