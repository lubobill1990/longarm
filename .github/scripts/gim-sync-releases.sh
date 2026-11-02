#!/usr/bin/env bash
# gim-sync-releases.sh: copies this repository's GitHub Releases into the
# Microsoft-internal copy gim-home/longarm - the same tags (v0.1.28 ...),
# the same files (signed binaries, packages, SHA256SUMS) - so the internal
# download links match the public ones version for version (the tray's
# "Add a machine" points an internal hub's machines there).
#
# It runs in gim-home, not here: gim-home's workflow
# (.github/gim-home/sync-releases.yml, committed there once) fetches this
# script from the public repository and runs it on a schedule with
# gim-home's own GITHUB_TOKEN - it reads the public releases and writes
# gim-home's, so no token of either side has to cross over
# (docs/ci-cd.md "Internal mirror").
#
#   gim-sync-releases.sh
#
# Env: GH_TOKEN (gim-home's GITHUB_TOKEN: contents write), SRC_REPO (the
#      public repository, [github.com/]OWNER/REPO), DST_REPO (default
#      $GITHUB_REPOSITORY), SYNC_COUNT (the newest N releases, default 5),
#      SYNC_TAG (only this tag, e.g. an older one).
#
# A release missing in gim-home is created with all files; one with fewer
# files than here (an interrupted run) gets them uploaded again. The tag
# points at gim-home's default branch: the built-in token can't push this
# repository's history there (its workflow files need a token with the
# Workflows permission), and the release notes name the public tag.
set -euo pipefail

src=${SRC_REPO:?gim-sync-releases.sh: set SRC_REPO (OWNER/REPO)}
src=${src#https://}
src=${src#github.com/}
dst=${DST_REPO:-${GITHUB_REPOSITORY:?gim-sync-releases.sh: set DST_REPO (OWNER/REPO)}}
count=${SYNC_COUNT:-5}
only=${SYNC_TAG:-}
summary=${GITHUB_STEP_SUMMARY:-/dev/null}
[[ $count =~ ^[0-9]+$ ]] || {
	echo "gim-sync-releases.sh: SYNC_COUNT '$count' is not a number" >&2
	exit 2
}
tag_re='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.]{0,31})?$'

# the public releases, newest first: tag, prerelease, number of files
if [[ -n $only ]]; then
	[[ $only =~ $tag_re ]] || {
		echo "gim-sync-releases.sh: SYNC_TAG '$only' is not vMAJOR.MINOR.PATCH[-pre]" >&2
		exit 2
	}
	want=$(gh api "repos/$src/releases/tags/$only" --jq '[.tag_name, (.prerelease|tostring), (.assets|length|tostring)]|@tsv')
else
	want=$(gh api "repos/$src/releases?per_page=$count" --jq '.[]|select(.draft|not)|[.tag_name, (.prerelease|tostring), (.assets|length|tostring)]|@tsv')
fi
latest=$(gh api "repos/$src/releases/latest" --jq .tag_name 2>/dev/null || true)
# gim-home's releases: tag, number of files
have=$(gh api "repos/$dst/releases?per_page=100" --paginate --jq '.[]|[.tag_name, (.assets|length|tostring)]|@tsv')
target=$(gh api "repos/$dst/commits/HEAD" --jq .sha) || {
	echo "::error title=gim-sync-releases::$dst has no commits: a release needs its default branch"
	exit 1
}

synced=0
# oldest first: the newest stable release is created last, as the latest
while IFS=$'\t' read -r tag pre n; do
	[[ -n $tag ]] || continue
	[[ $tag =~ $tag_re ]] || {
		echo "skipping $tag: not vMAJOR.MINOR.PATCH[-pre]"
		continue
	}
	got=$(awk -F'\t' -v t="$tag" '$1 == t { print $2 }' <<<"$have")
	if [[ -n $got && $got -ge $n ]]; then
		echo "$tag: in $dst with all $n files"
		continue
	fi
	dir=$(mktemp -d)
	gh release download "$tag" --repo "$src" --dir "$dir"
	if [[ -z $got ]]; then
		flags=(--target "$target" --title "Longarm ${tag#v}"
			--notes "The internal copy of https://github.com/$src/releases/tag/$tag - the same files (signed binaries, packages, SHA256SUMS).")
		if [[ $pre == true ]]; then
			flags+=(--prerelease)
		elif [[ $tag == "$latest" ]]; then
			flags+=(--latest)
		else
			flags+=(--latest=false)
		fi
		gh release create "$tag" --repo "$dst" "${flags[@]}" "$dir"/*
		echo "$tag: created in $dst with $n files" | tee -a "$summary"
	else
		gh release upload "$tag" --repo "$dst" --clobber "$dir"/*
		echo "$tag: $got of $n files in $dst, uploaded again" | tee -a "$summary"
	fi
	rm -rf "$dir"
	synced=$((synced + 1))
done < <(awk '{ l[NR] = $0 } END { for (i = NR; i > 0; i--) print l[i] }' <<<"$want")
echo "gim-sync-releases: $synced release(s) synced from $src to $dst" | tee -a "$summary"
