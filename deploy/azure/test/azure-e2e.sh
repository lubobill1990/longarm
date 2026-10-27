#!/usr/bin/env bash
# azure-e2e.sh: end-to-end test on real Azure (.github/workflows/azure-e2e.yml).
#
# A fresh hub (deploy.sh, storage-auth identity, Entra sign-in for the
# test tenant's app) in a shared resource group; this machine signs in with
# the Azure CLI's current identity (in CI: the job's managed identity,
# CI_APP_ID, mapped to the Longarm user "ci" by --entra-apps), registers itself
# as an owned worker through the hub's relay (a CI runner is behind NAT),
# and runs work there - on that worker and through the scheduler.
# Cleanup is azure-e2e-cleanup.sh (the workflow runs it always).
#
#   NAME=lae2e123 IMAGE=ghcr.io/<owner>/longarm-hub:sha-<12> BIN=<dir with longarm, longarmd> \
#   RG=longarm-ci ENTRA_TENANT=<tenant> ENTRA_CLIENT=<app id> [CI_APP_ID=<identity client id>] #   [EXPECT_USER=ci] deploy/azure/test/azure-e2e.sh
# Git Bash on Windows (local runs): keep resource IDs as they are, not as paths
export MSYS_NO_PATHCONV=1
set -euo pipefail

: "${NAME:?}" "${IMAGE:?}" "${BIN:?}" "${RG:?}" "${ENTRA_TENANT:?}" "${ENTRA_CLIENT:?}"
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
client=$work/client worker=$work/worker
mkdir -p "$client" "$worker"
longarm() { "$BIN/longarm" --config-dir "$client" "$@"; }

step() { echo "::group::$*"; }
done_step() { echo "::endgroup::"; }
fail() {
	echo "::error::azure-e2e: $*"
	[[ ! -f $work/longarmd.log ]] || { echo "--- longarmd log"; tail -n 80 "$work/longarmd.log"; }
	echo "--- hub log"
	az container logs -g "$RG" -n "$NAME" --container-name hub 2>/dev/null | tail -n 80 || true
	exit 1
}
# retry N SECONDS CMD...: until CMD succeeds, at most N tries
retry() {
	local n=$1 s=$2
	shift 2
	for ((i = 1; i <= n; i++)); do
		"$@" && return 0
		sleep "$s"
	done
	return 1
}

step "deploy hub $NAME ($IMAGE)"
"$here/../deploy.sh" --name "$NAME" --resource-group "$RG" --storage-auth identity --no-panel \
	--image "$IMAGE" --entra-tenant "$ENTRA_TENANT" --entra-client "$ENTRA_CLIENT" --entra-web off \
	--entra-apps "${CI_APP_ID:-$ENTRA_CLIENT}=ci" --timeout 1200 | tee "$work/deploy.out"
hub=$(sed -n 's/^  hub address *//p' "$work/deploy.out")
fp=$(sed -n 's/^  CA fingerprint *//p' "$work/deploy.out")
fqdn=${hub%:*}
[[ -n $hub && ${#fp} -eq 64 ]] || fail "no hub address / CA fingerprint in the deploy output"
done_step

step "longarm login (the Azure CLI's identity -> Entra token -> certificate)"
retry 3 10 longarm login "$hub" --ca-fingerprint "$fp" || fail "longarm login"
who=$(longarm whoami)
echo "$who"
grep -q "user ${EXPECT_USER:-ci}" <<<"$who" || fail "signed in as someone else than ${EXPECT_USER:-ci}: $who"
done_step

step "register this machine as an owned worker"
wname=ci-$(printf '%s' "${GITHUB_RUN_ID:-$$}" | tail -c 10)
code=$(longarm worker add "$wname" | head -n 1)
[[ $code =~ ^[0-9A-Z]{3}-[0-9A-Z]{3}$ ]] || fail "worker add printed no code: $code"
"$BIN/longarmd" join "$hub" "$code" --ca-fingerprint "$fp" --name "$wname" --data-dir "$worker" | tee "$work/join.out"
grep -q "owned by ${EXPECT_USER:-ci}" "$work/join.out" || fail "the worker is not owned by ${EXPECT_USER:-ci}"
# behind NAT: through the hub's relay (UDP 7445, TLS fallback on 7443)
"$BIN/longarmd" run --data-dir "$worker" \
	--listen "longnet://$fqdn:7445?tcp=$fqdn:7443&name=$wname" >"$work/longarmd.log" 2>&1 &
echo $! >"$work/longarmd.pid"
online() { longarm worker ls | grep -E "^$wname +mine +${EXPECT_USER:-ci} .*online" >/dev/null; }
retry 60 5 online || fail "the worker did not come online"
longarm worker ls
done_step

step "run on the chosen worker, and through the scheduler"
# shellcheck disable=SC2016 # $(hostname) is for the worker's shell
out=$(retry 3 10 longarm exec "$wname" -- sh -c 'echo "hello from $(hostname)"') || fail "exec on $wname"
echo "$out"
grep -q "hello from" <<<"$out" || fail "exec output: $out"
out=$(longarm run -f -- sh -c 'echo placed-by-scheduler' 2>&1) || fail "longarm run: $out"
echo "$out"
grep -q "placed on $wname" <<<"$out" || fail "the scheduler did not place the job on the only usable worker"
grep -q "placed-by-scheduler" <<<"$out" || fail "job output missing"
done_step

step "the worker is the owner's only"
# app-only identities never get admin rights; the hub refuses to share
# without can_share
if longarm worker share "$wname" --everyone 2>"$work/share.err"; then
	fail "an app user without can_share shared its worker"
fi
cat "$work/share.err"
done_step

kill "$(cat "$work/longarmd.pid")" 2>/dev/null || true
echo "azure-e2e: ok"
