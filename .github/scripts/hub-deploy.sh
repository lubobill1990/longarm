#!/usr/bin/env bash
# hub-deploy.sh: run longarm-hub-deploy on the hub through the deploy user's SSH
# forced command (deploy/hub/longarm-hub-deploy; docs/ci-cd.md). The key can do
# nothing else there. Secrets come from the environment and live in a
# private temp dir until the script exits. Never run with -x.
#
#   hub-deploy.sh check <version> | deploy <version> | rollback | status
# Env: HUB_SSH (user@host), HUB_DEPLOY_SSH_KEY (private key), HUB_SSH_KNOWN_HOSTS;
#      HUB_DEPLOY_SSH_COMMAND (tests: the ssh binary, default ssh)
set -euo pipefail
umask 077

die() {
	echo "::error::$*" >&2
	exit 1
}
[[ $# -ge 1 ]] || die "usage: hub-deploy.sh <longarm-hub-deploy command>"
[[ -n ${HUB_SSH:-} ]] || die "repository variable HUB_SSH is not set"
[[ -n ${HUB_DEPLOY_SSH_KEY:-} ]] || die "secret HUB_DEPLOY_SSH_KEY is not set (or this job is not in the release environment)"
[[ -n ${HUB_SSH_KNOWN_HOSTS:-} ]] || die "secret HUB_SSH_KNOWN_HOSTS is not set"
ssh=${HUB_DEPLOY_SSH_COMMAND:-ssh}
command -v "$ssh" >/dev/null 2>&1 || die "ssh is not installed on this runner (openssh-client)"

sec=$(mktemp -d "${RUNNER_TEMP:-/tmp}/hub-deploy.XXXXXX")
trap 'rm -rf "$sec"' EXIT
printf '%s\n' "$HUB_DEPLOY_SSH_KEY" | tr -d '\r' >"$sec/id"
printf '%s\n' "$HUB_SSH_KNOWN_HOSTS" | tr -d '\r' >"$sec/known_hosts"

# No config files, no agent, only this key, only the pinned host key.
"$ssh" -F /dev/null -T -i "$sec/id" \
	-o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes \
	-o StrictHostKeyChecking=yes -o UserKnownHostsFile="$sec/known_hosts" -o GlobalKnownHostsFile=/dev/null \
	-o ConnectTimeout=20 -o ServerAliveInterval=15 -o ServerAliveCountMax=8 \
	-- "$HUB_SSH" "$*"
