#!/usr/bin/env bash
# ci-job.sh: the scheduled end-to-end test on real Azure (deploy/azure/ci/ci-job.bicep).
# Runs in an Azure Container Apps job with a managed identity, from a fresh
# clone of the branch under test in /src: builds longarm and longarmd, waits
# for the branch commit's hub image, runs azure-e2e.sh as the identity and
# cleans up. With GITHUB_STATUS_TOKEN the result becomes the commit's
# "azure-e2e" status on GitHub.
#
# Environment (set by the job): REPO, BRANCH, AZURE_CLIENT_ID (the managed
# identity), RG, ENTRA_TENANT, ENTRA_CLIENT [, GITHUB_STATUS_TOKEN, HUB_IMAGE].
set -uo pipefail

: "${REPO:?}" "${AZURE_CLIENT_ID:?}" "${RG:?}" "${ENTRA_TENANT:?}" "${ENTRA_CLIENT:?}"
src=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$src" || exit 1
commit=$(git rev-parse HEAD)
started=$(date +%s)

# status STATE DESCRIPTION: the commit's azure-e2e status on GitHub (best effort)
status() {
	[[ -n ${GITHUB_STATUS_TOKEN:-} ]] || return 0
	python3 - "$1" "$2" <<'EOF' | curl -fsS -o /dev/null -X POST -H "Authorization: Bearer $GITHUB_STATUS_TOKEN" \
		-H "Accept: application/vnd.github+json" --data @- \
		"https://api.github.com/repos/$REPO/statuses/$commit" || echo "ci-job: could not set the GitHub status"
import json, sys
print(json.dumps({"state": sys.argv[1], "context": "azure-e2e", "description": sys.argv[2][:140]}))
EOF
}

finish() {
	local rc=$1 msg=$2
	echo "ci-job: $msg ($(($(date +%s) - started)) s)"
	if ((rc == 0)); then status success "$msg"; else status failure "$msg"; fi
	exit "$rc"
}

# the runner image (azure-cli on Azure Linux) is minimal: deploy.sh and the
# tests need these too
tdnf install -y gawk sed grep findutils procps-ng diffutils coreutils >/dev/null 2>&1 ||
	echo "ci-job: tdnf could not install the base tools"

echo "ci-job: $REPO@$commit"
status pending "running on Azure"

echo "::group::go and the binaries"
gover=$(sed -n 's/^go //p' go.mod | head -n 1)
curl -fsSL "https://go.dev/dl/go${gover}.linux-amd64.tar.gz" | tar -xz -C /usr/local ||
	finish 1 "could not install Go $gover"
export PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=auto
bin=$(mktemp -d)
go build -trimpath -o "$bin/" ./cmd/longarm ./cmd/longarmd || finish 1 "build failed"
echo "::endgroup::"

az login --identity --client-id "$AZURE_CLIENT_ID" -o none || finish 1 "az login --identity failed"

# the hub image of this commit (image.yml publishes it after a push to main)
owner=${REPO%%/*}
repo_path=${owner,,}/longarm-hub
image=${HUB_IMAGE:-ghcr.io/$repo_path:sha-${commit::12}}
tag=${image##*:}
for _ in $(seq 1 60); do
	tok=$(curl -fsS "https://ghcr.io/token?scope=repository:$repo_path:pull" | python3 -c 'import json,sys;print(json.load(sys.stdin)["token"])')
	if curl -fsS -o /dev/null -H "Authorization: Bearer $tok" -H "Accept: application/vnd.oci.image.index.v1+json" \
		"https://ghcr.io/v2/$repo_path/manifests/$tag"; then
		break
	fi
	sleep 30
done

name=lae2e$(date -u +%m%d%H%M)
"$src/deploy/azure/test/azure-e2e-cleanup.sh" "$RG" --older 6h
NAME=$name IMAGE=$image BIN=$bin CI_APP_ID=$AZURE_CLIENT_ID "$src/deploy/azure/test/azure-e2e.sh"
rc=$?
"$src/deploy/azure/test/azure-e2e-cleanup.sh" "$RG" "$name"
if ((rc == 0)); then
	finish 0 "azure-e2e passed ($name)"
fi
finish 1 "azure-e2e failed ($name): see the job's logs"
