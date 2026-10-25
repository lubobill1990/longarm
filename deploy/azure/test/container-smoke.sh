#!/usr/bin/env bash
# container-smoke.sh <image> <longarm binary>
# The longarm-hub image as deploy/azure runs it, with Azurite standing in for
# Blob Storage and a docker volume for the Azure Files share (CI:
# .github/workflows/image.yml; needs docker and the Azure CLI):
#   1. first start: restore (nothing), bootstrap (CA, hub.toml, admin code),
#      health, an admin joins with the printed code, Litestream replicates
#   2. container replaced (docker stop = SIGTERM, rm; the local database is
#      gone): the new one restores it from the replica - writes made just
#      before the stop are there, the CA and the admin's certificate still work
#   3. a replica that can't be read stops the start
set -euo pipefail

image=${1:?usage: container-smoke.sh <image> <longarm binary>}
longarm=${2:?usage: container-smoke.sh <image> <longarm binary>}
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
		for c in "$p-hub" "$p-hub2" "$p-hub3"; do
			if docker inspect "$c" >/dev/null 2>&1; then
				echo "::group::docker logs $c"
				docker logs "$c" 2>&1 | tail -n 200
				echo "::endgroup::"
			fi
		done
	fi
	docker rm -f "$p-hub" "$p-hub2" "$p-hub3" "$p-azurite" >/dev/null 2>&1 || true
	docker volume rm "$p-share" >/dev/null 2>&1 || true
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
az storage container exists -n litestream --connection-string "$conn" -o tsv | grep -qx True || fail "azurite: no container"

# hub_run NAME [extra docker args]: the environment of deploy/azure/main.bicep
hub_run() {
	local name=$1
	shift
	docker run -d --name "$name" --network "$p" \
		-p 127.0.0.1:7443:7443/tcp -p 127.0.0.1:7443:7443/udp -p 127.0.0.1:7444:7444/udp \
		-v "$p-share:/data" \
		-e LONGARM_HUB_DATA_DIR=/data \
		-e LONGARM_HUB_CONFIG=/data/hub.toml \
		-e LONGARM_HUB_DB=/var/lib/longarm-hub/hub.db \
		-e LONGARM_HUB_ADVERTISE=127.0.0.1 \
		-e LONGARM_HUB_HTTP=127.0.0.1:7480 \
		-e LONGARM_HUB_BOOTSTRAP=1 \
		-e LONGARM_HUB_BOOTSTRAP_TTL=24h \
		-e LONGARM_HUB_LITESTREAM_CONFIG=/etc/longarm-hub/litestream-abs.yml \
		-e LONGARM_HUB_BLOB_ACCOUNT=$acct \
		-e LONGARM_HUB_BLOB_CONTAINER=litestream \
		-e LONGARM_HUB_BLOB_ENDPOINT=http://azurite:10000/$acct \
		-e LITESTREAM_AZURE_ACCOUNT_KEY=$key \
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

la() { LONGARM_CONFIG_DIR=$work/laptop "$longarm" "$@"; }

step "first start"
hub_run "$p-hub"
wait_healthy "$p-hub"
fp=$(bootstrap "$p-hub" ca_fingerprint)
code=$(bootstrap "$p-hub" admin_join_code)
addr=$(bootstrap "$p-hub" hub_address)
[[ ${#fp} -eq 64 && -n $code && $addr == 127.0.0.1:7443 ]] || fail "bootstrap line: fp=$fp code=$code addr=$addr"
[[ $(bootstrap "$p-hub" admin_joined) == false ]] || fail "admin_joined on a fresh hub"
docker logs "$p-hub" 2>&1 | grep -q "no matching backups found" || fail "first start: no empty-replica restore"
docker exec "$p-hub" /usr/local/bin/longarm-hub ca show | grep -q "$fp" || fail "ca show"
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
docker exec -i "$p-hub" /usr/local/bin/longarm-hub user add bob --password-stdin <<<'another passphrase' >/dev/null

step "replace the container (SIGTERM, rm)"
docker stop -t 60 "$p-hub" >/dev/null
docker logs "$p-hub" 2>&1 | grep -q "longarm-hub stopped" || fail "hub did not stop gracefully"
docker logs "$p-hub" 2>&1 | grep -q "litestream shut down" || fail "litestream did not shut down"
[[ $(docker inspect -f '{{.State.ExitCode}}' "$p-hub") == 0 ]] || fail "exit code $(docker inspect -f '{{.State.ExitCode}}' "$p-hub")"
docker rm "$p-hub" >/dev/null
n=$(az storage blob list -c litestream --connection-string "$conn" --query "length(@)" -o tsv)
[[ $n -gt 0 ]] || fail "no replica in blob storage"

hub_run "$p-hub2"
wait_healthy "$p-hub2"
docker logs "$p-hub2" 2>&1 | grep -q "no matching backups found" && fail "hub2 found no replica"
[[ $(bootstrap "$p-hub2" ca_fingerprint) == "$fp" ]] || fail "CA changed"
[[ $(bootstrap "$p-hub2" admin_joined) == true ]] || fail "the restored database lost the admin"
[[ -z $(bootstrap "$p-hub2" admin_join_code) ]] || fail "a new admin code after the admin joined"
users=$(docker exec "$p-hub2" /usr/local/bin/longarm-hub user ls)
grep -qx alice <<<"$users" || fail "alice lost: $users"
grep -qx bob <<<"$users" || fail "bob (written just before the stop) lost: $users"
la admin join-code --role worker >/dev/null || fail "admin certificate after the replacement"
docker rm -f "$p-hub2" >/dev/null

step "unreadable replica: no start"
hub_run "$p-hub3" -e LITESTREAM_AZURE_ACCOUNT_KEY=d3Jvbmcta2V5 --restart no
for _ in $(seq 1 60); do
	[[ $(docker inspect -f '{{.State.Running}}' "$p-hub3") == true ]] || break
	sleep 1
done
[[ $(docker inspect -f '{{.State.Running}}' "$p-hub3") == false ]] || fail "started with a wrong storage key"
docker logs "$p-hub3" 2>&1 | grep -q "litestream restore" || fail "no restore error in the log"

echo "container-smoke: ok"
