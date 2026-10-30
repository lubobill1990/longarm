#!/usr/bin/env bash
# release-go-cache.sh warm|stats <goos/goarch>...
#
# The compile units of the release builds, as .goreleaser.yaml compiles them
# (CGO_ENABLED=0, -trimpath; the tray with -tags production), plus
# packaging/hubrelease and the longarm that publish / rollout / the image
# smoke test build (go build -trimpath, CGO_ENABLED=0). docs/ci-cd.md "Caches".
#
#   warm   ci.yml, push to main: compile them all into GOCACHE, then drop the
#          cache entries this run didn't use (so the saved cache holds the
#          release's compile units and nothing else)
#   stats  release.yml, after restoring the cache: how many of the non-main
#          packages are already in GOCACHE (go list's Stale), per target
#
# Only the compile step is cached (link outputs never are, and a main
# package's compile carries the version / ldflags in its build info), so
# ldflags don't need to match; flags, tags, CGO, GOOS/GOARCH and the Go
# version do.
set -euo pipefail

mode=${1:?usage: release-go-cache.sh warm|stats <goos/goarch>...}
shift
[[ $# -gt 0 ]] || set -- "$(go env GOHOSTOS)/$(go env GOHOSTARCH)"
export CGO_ENABLED=0
pkgs=(./cmd/longarm ./cmd/longarmd ./cmd/longarm-hub ./cmd/longarm-relay ./packaging/hubrelease)

native() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s\n' "$1"; fi; }

case $mode in
warm)
	start=$(date +%s)
	out=$(mktemp -d "${RUNNER_TEMP:-/tmp}/release-go-cache.XXXXXX")
	trap 'rm -rf "$out"' EXIT
	for t in "$@"; do
		export GOOS=${t%/*} GOARCH=${t#*/}
		echo "::group::warm $t"
		mkdir -p "$out/$GOOS-$GOARCH"
		go build -trimpath -o "$(native "$out/$GOOS-$GOARCH")/" "${pkgs[@]}"
		if [[ $GOOS == windows && -d cmd/longarm-tray ]]; then
			go build -trimpath -tags production -o "$(native "$out/$GOOS-$GOARCH")/" ./cmd/longarm-tray
		fi
		echo "::endgroup::"
	done
	# Go refreshes an entry's mtime when it uses it, unless the mtime is less
	# than an hour old: everything older than (start - 1 h) went unused.
	gocache=$(go env GOCACHE)
	if command -v cygpath >/dev/null 2>&1; then gocache=$(cygpath -u "$gocache"); fi
	before=$(du -sm "$gocache" | cut -f1)
	find "$gocache" -type f ! -newermt "@$((start - 3660))" -delete
	echo "GOCACHE $gocache: ${before} MB -> $(du -sm "$gocache" | cut -f1) MB after dropping the unused entries"
	;;
stats)
	total=0 hits=0
	report=""
	for t in "$@"; do
		export GOOS=${t%/*} GOARCH=${t#*/}
		lists=("$(go list -trimpath -deps -f '{{if ne .Name "main"}}{{if .Stale}}miss {{.ImportPath}}{{else}}hit{{end}}{{end}}' "${pkgs[@]}")")
		if [[ $GOOS == windows && -d cmd/longarm-tray ]]; then
			lists+=("$(go list -trimpath -tags production -deps -f '{{if ne .Name "main"}}{{if .Stale}}miss {{.ImportPath}}{{else}}hit{{end}}{{end}}' ./cmd/longarm-tray)")
		fi
		all=$(printf '%s\n' "${lists[@]}" | grep -v '^$' || true)
		n=$(grep -c . <<<"$all" || true)
		h=$(grep -c '^hit$' <<<"$all" || true)
		total=$((total + n)) hits=$((hits + h))
		report+="| $t | $h / $n |"$'\n'
		misses=$(grep '^miss ' <<<"$all" | cut -d' ' -f2 || true)
		if [[ -n $misses ]]; then
			echo "$t: $((n - h)) packages to compile, e.g.:"
			head -n 10 <<<"$misses" | sed 's/^/  /'
		fi
	done
	pct=0
	[[ $total -eq 0 ]] || pct=$((100 * hits / total))
	summary="Go build cache: $hits of $total packages compiled already ($pct %)"
	echo "$summary"
	if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
		{
			echo "### $summary"
			echo
			echo "| target | in the cache |"
			echo "|---|---|"
			printf '%s' "$report"
		} >>"$GITHUB_STEP_SUMMARY"
	fi
	;;
*)
	echo "usage: release-go-cache.sh warm|stats <goos/goarch>..." >&2
	exit 2
	;;
esac
