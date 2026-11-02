#!/usr/bin/env bash
# gim-release.sh: the release of <tag> in the Microsoft-internal copy
# (gim-home/longarm) with the same files as this repository's release - the
# same signed binaries, packages and SHA256SUMS, at the same commit and tag
# (docs/ci-cd.md "Internal mirror"). release.yml's publish job runs it for
# every release and release candidate; gim-release.yml re-runs it by hand
# (an older release, a failed run).
#
#   gim-release.sh <tag> [--prerelease] -- <file>...
#
# Env: GIM_MIRROR_TOKEN (the mirror's PAT: Contents read/write on the
#      target; unset: a notice, exit 0), SHA (the tag's commit, default
#      $GITHUB_SHA), SRC_REPO (default $GITHUB_REPOSITORY), DST_REPO
#      (default gim-home/longarm), GIM_WAIT_SECONDS (how long to wait for
#      the mirror to carry the commit over, default 1200).
#
# The commit must exist in the target first: mirror-gim.yml carries
# main and the branches over within minutes (and every 15 minutes), so
# this waits for it rather than pushing history itself.
set -euo pipefail

tag=${1:?usage: gim-release.sh <tag> [--prerelease] -- <file>...}
shift
prerelease=false
while [[ $# -gt 0 && $1 != -- ]]; do
	case $1 in
	--prerelease) prerelease=true ;;
	*)
		echo "gim-release.sh: unknown option $1" >&2
		exit 2
		;;
	esac
	shift
done
[[ ${1:-} == -- ]] && shift
[[ $# -gt 0 ]] || {
	echo "gim-release.sh: no files" >&2
	exit 2
}
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.]{0,31})?$ ]] || {
	echo "gim-release.sh: tag '$tag' is not vMAJOR.MINOR.PATCH[-pre]" >&2
	exit 2
}

dst=${DST_REPO:-gim-home/longarm}
src=${SRC_REPO:-${GITHUB_REPOSITORY:?gim-release.sh: set SRC_REPO (owner/repo)}}
sha=${SHA:-${GITHUB_SHA:-}}
[[ $sha =~ ^[0-9a-f]{40}$ ]] || {
	echo "gim-release.sh: SHA '$sha' is not a commit id" >&2
	exit 2
}
summary=${GITHUB_STEP_SUMMARY:-/dev/null}
if [[ -z ${GIM_MIRROR_TOKEN:-} ]]; then
	echo "::notice title=gim-release::secret GIM_MIRROR_TOKEN is not set: no release $tag in $dst (docs/ci-cd.md, Internal mirror)"
	exit 0
fi
export GH_TOKEN=$GIM_MIRROR_TOKEN

# the commit in the target (the mirror may still be carrying it over)
wait_s=${GIM_WAIT_SECONDS:-1200}
deadline=$((SECONDS + wait_s))
until gh api "repos/$dst/commits/$sha" --silent 2>/dev/null; do
	if ((SECONDS >= deadline)); then
		echo "::error title=gim-release::commit $sha of $tag is not in $dst after ${wait_s}s: run mirror-gim.yml, then gim-release.yml for $tag"
		exit 1
	fi
	echo "waiting for the mirror to carry $sha over to $dst ..."
	sleep 30
done

version=${tag#v}
if gh release view "$tag" --repo "$dst" >/dev/null 2>&1; then
	echo "release $tag exists in $dst: uploading its files again"
else
	flags=(--target "$sha" --title "Longarm $version"
		--notes "The internal copy of https://github.com/$src/releases/tag/$tag - the same files (signed binaries, packages, SHA256SUMS).")
	if [[ $prerelease == true ]]; then flags+=(--prerelease); fi
	gh release create "$tag" --repo "$dst" "${flags[@]}"
fi
gh release upload "$tag" --repo "$dst" --clobber "$@"
echo "gim-release: $tag in $dst with $# files" | tee -a "$summary"
