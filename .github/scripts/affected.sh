#!/usr/bin/env bash
# affected.sh: does a release change a component since a version that runs?
# (release.yml "affected": the hub deploys and the worker rollout only when
# their code changed - docs/ci-cd.md "Release channels".)
#
#   affected.sh paths hub|workers               the component's pathspecs, one a line
#   affected.sh changed hub|workers FROM TO     exit 0: changed (FROM unknown counts as changed),
#                                               1: unchanged; the changed files on stderr
#
# hub: the Go packages longarm-hub and longarm-relay build from (go list -deps),
#      deploy/hub, and the web UI it serves (not the tray's pages, not the built
#      dist - its file names change with every web change).
# workers: what a rollout installs: longarmd, longarm, longarm-tray,
#      longarm-update, the MSI helper and packaging, the whole web UI.
# Tests never count.
set -euo pipefail

die() {
	echo "affected.sh: $*" >&2
	exit 2
}

# go_dirs CMD...: the main module's package directories CMD... build from
go_dirs() {
	local root
	root=$(go list -m -f '{{.Dir}}' | tr '\\' '/')
	go list -deps -f '{{if .Module}}{{if .Module.Main}}{{.Dir}}{{end}}{{end}}' "$@" |
		tr '\\' '/' | while IFS= read -r d; do printf '%s\n' "${d#"$root"/}"; done | sort -u
}

paths() {
	case $1 in
	hub)
		go_dirs ./cmd/longarm-hub ./cmd/longarm-relay
		printf '%s\n' go.mod go.sum deploy/hub web \
			':(exclude)web/src/tray' ':(exclude)web/shots' ':(exclude)web/e2e' ':(exclude)internal/webui/dist'
		;;
	workers)
		go_dirs ./cmd/longarmd ./cmd/longarm ./cmd/longarm-tray ./cmd/longarm-update ./cmd/longarm-msihelper
		printf '%s\n' go.mod go.sum packaging web skills ':(exclude)web/shots' ':(exclude)web/e2e'
		;;
	*) die "unknown component $1 (hub, workers)" ;;
	esac
	printf '%s\n' ':(exclude,glob)**/*_test.go' ':(exclude,glob)**/*.test.ts' ':(exclude,glob)**/*.test.tsx'
}

case ${1:-} in
paths)
	[[ $# -eq 2 ]] || die "usage: affected.sh paths hub|workers"
	paths "$2"
	;;
changed)
	[[ $# -eq 4 ]] || die "usage: affected.sh changed hub|workers FROM TO"
	comp=$2 from=$3 to=$4
	if ! git rev-parse -q --verify "$from^{commit}" >/dev/null; then
		echo "affected.sh: $from is not known here: $comp counts as changed" >&2
		exit 0
	fi
	mapfile -t specs < <(paths "$comp")
	files=$(git diff --name-only "$from" "$to" -- "${specs[@]}")
	if [[ -z $files ]]; then
		echo "affected.sh: $comp unchanged since $from" >&2
		exit 1
	fi
	echo "affected.sh: $comp changed since $from ($(wc -l <<<"$files") files):" >&2
	head -20 <<<"$files" >&2
	exit 0
	;;
*) die "usage: affected.sh paths|changed ..." ;;
esac
