#!/usr/bin/env bash
# longarm-release.sh: the CI's side of a release, with the release-role
# certificate (docs/ci-cd.md). Secrets come from the environment, are written
# to a private temp dir (umask 077) and deleted when the script exits. Never
# run with -x.
#
#   longarm-release.sh sign <longarm> <dir> <version>                sign <dir> only (no hub needed)
#   longarm-release.sh publish <longarm> <dir> <version> <channel>   sign <dir> + upload it to the hub
#   longarm-release.sh rollout <longarm> <version> <canary-wait> [push flags...]
#                                                           canary rollout, follow it; fail if it halts
# Env: LONGARM_HUB (host:port), LONGARM_RELEASE_TLS (base64 tar.gz: ca.crt node.crt node.key),
#      LONGARM_RELEASE_KEY (sign, publish: minisign private key file content),
#      LONGARM_RELEASE_KEY_PASSWORD (optional, encrypted key)
set -euo pipefail
umask 077

die() {
	echo "::error::$*" >&2
	exit 1
}

[[ $# -ge 2 ]] || die "usage: longarm-release.sh sign|publish|rollout <longarm> ..."
cmd=$1 longarm=$2
shift 2
[[ -x $longarm ]] || chmod +x "$longarm"

sec=$(mktemp -d "${RUNNER_TEMP:-/tmp}/longarm-release.XXXXXX")
trap 'rm -rf "$sec"' EXIT
mkdir -p "$sec/cfg/tls"

if [[ $cmd == sign ]]; then
	[[ $# -eq 2 ]] || die "usage: longarm-release.sh sign <longarm> <dir> <version>"
	dir=$1 version=$2
	[[ -n ${LONGARM_RELEASE_KEY:-} ]] || die "secret LONGARM_RELEASE_KEY is not set"
	printf '%s\n' "$LONGARM_RELEASE_KEY" >"$sec/longarm-release.key"
	"$longarm" --config-dir "$sec/cfg" admin release sign "$dir" --version "$version" --key "$sec/longarm-release.key"
	exit 0
fi

[[ -n ${LONGARM_HUB:-} ]] || die "repository variable LONGARM_HUB is not set"
[[ -n ${LONGARM_RELEASE_TLS:-} ]] || die "secret LONGARM_RELEASE_TLS is not set (or this job is not in the release environment)"
# The bundle: exactly ca.crt, node.crt, node.key.
if ! base64 -d <<<"$LONGARM_RELEASE_TLS" | tar -xzf - -C "$sec/cfg/tls" --no-same-owner --no-same-permissions; then
	die "LONGARM_RELEASE_TLS is not a base64 tar.gz"
fi
for f in ca.crt node.crt node.key; do
	[[ -f $sec/cfg/tls/$f ]] || die "LONGARM_RELEASE_TLS lacks $f (tar -C <bundle dir> -czf - ca.crt node.crt node.key | base64 -w0)"
done
chmod 0600 "$sec/cfg/tls/node.key"
printf 'address = "%s"\n' "$LONGARM_HUB" >"$sec/cfg/hub.toml"
lac() { "$longarm" --config-dir "$sec/cfg" "$@"; }

case $cmd in
publish)
	[[ $# -eq 3 ]] || die "usage: longarm-release.sh publish <longarm> <dir> <version> <channel>"
	dir=$1 version=$2 channel=$3
	[[ -n ${LONGARM_RELEASE_KEY:-} ]] || die "secret LONGARM_RELEASE_KEY is not set"
	printf '%s\n' "$LONGARM_RELEASE_KEY" >"$sec/longarm-release.key"
	# Signs every release file in <dir> (<file>.minisig, bound to name and
	# version) and uploads them; the hub checks the signatures against its
	# public_key. A re-run with the same files is a no-op.
	lac admin release publish "$dir" --version "$version" --channel "$channel" \
		--key "$sec/longarm-release.key" --skip-identical
	;;
rollout)
	[[ $# -ge 2 ]] || die "usage: longarm-release.sh rollout <longarm> <version> <canary-wait> [push flags...]"
	version=$1 wait=$2
	shift 2
	# A re-run follows (or accepts) the rollout of this version instead of
	# starting it again.
	st=$(lac admin release status --json)
	cur_v=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' <<<"$st" | head -n 1)
	cur_s=$(sed -n 's/^ *"state": *"\([^"]*\)".*/\1/p' <<<"$st" | head -n 1)
	if [[ $cur_v == "$version" ]]; then
		case $cur_s in
		done)
			echo "rollout of $version is already done"
			lac admin release status
			exit 0
			;;
		canary | rolling)
			echo "rollout of $version is running: following it"
			lac admin release status -f
			exit $?
			;;
		esac
	fi
	# Canary first; the rest follow once it stayed healthy for <wait>.
	# -f exits non-zero when the rollout halts (a worker rolled back) or is
	# aborted.
	lac admin release push "$version" --canary 1 --wait "$wait" -f "$@"
	;;
*) die "unknown command $cmd" ;;
esac
