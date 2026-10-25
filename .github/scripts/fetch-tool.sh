#!/usr/bin/env bash
# fetch-tool.sh <name> <version> <url of a .tar.gz> <sha256> <path of the binary in the archive>
# A pinned tool: the archive is downloaded once per runner instance into
# $LONGARM_TOOLS (.github/actions/go-setup), its sha256 is checked on every
# use, and the binary is extracted into this job's temp dir. Prints that
# directory (also added to GITHUB_PATH for the following steps).
set -euo pipefail
name=$1 version=$2 url=$3 sum=$4 member=$5
cache=${LONGARM_TOOLS:?run .github/actions/go-setup first}/$name-$version.tar.gz
ok() { echo "$sum  $cache" | sha256sum -c --quiet - >/dev/null 2>&1; }
if ! ok; then
	mkdir -p "$(dirname "$cache")"
	curl -fsSL --retry 3 -o "$cache.tmp.$$" "$url"
	mv -f "$cache.tmp.$$" "$cache"
	ok || {
		rm -f "$cache"
		echo "::error::$url: sha256 mismatch" >&2
		exit 1
	}
fi
dir=$(mktemp -d "${RUNNER_TEMP:-/tmp}/$name.XXXXXX")
tar -xzf "$cache" -C "$dir" "$member"
[[ $member == "$name" ]] || mv "$dir/$member" "$dir/$name"
chmod 0755 "$dir/$name"
if [[ -n ${GITHUB_PATH:-} ]]; then echo "$dir" >>"$GITHUB_PATH"; fi
"$dir/$name" --version >"$dir/version.txt" 2>&1 || true
head -n 2 "$dir/version.txt" >&2
echo "$dir"
