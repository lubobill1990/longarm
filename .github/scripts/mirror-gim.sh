#!/usr/bin/env bash
# mirror-gim.sh: mirror this repository into the Microsoft-internal copy
# (gim-home/longarm) for mirror-gim.yml; docs/ci-cd.md "Internal mirror".
# Every run reconciles the whole state, whatever event started it:
#
#   branches  every branch but main, as it is (forced); open fork pull
#             requests' heads (refs/pull/N/head) as pr-N - git objects only,
#             nothing of them is run. Not a fork PR that changes .github/:
#             there it would be a same-repository branch, and a workflow
#             in it would run with that repository's token and secrets
#   main      the target's main carries extra commits (Start Right governance
#             files): our main is merged into it ("Sync main from <src>"),
#             always a fast-forward there, never forced
#   PRs       one PR there per open PR here, same head branch (pr-N for
#             forks), title and body + the footer "Mirrored from <src>#N";
#             closed there once closed or merged here. Never merged there.
#   deletes   a branch gone here is deleted there - but only one the mirror
#             pushed (its tip is a commit of this repository), never main,
#             dependabot/* or the head of an open PR native to the target
#
# PRs without the footer, and their branches, belong to the target (its own
# Dependabot, ...): never edited, closed, overwritten or deleted. Tags are
# not mirrored. Never run with -x.
#
# Env: GIM_MIRROR_TOKEN (PAT for the target; unset: notice, exit 0),
#      SRC_TOKEN (reads this repository; optional for a public one),
#      SRC_REPO (default $GITHUB_REPOSITORY), DST_REPO (default gim-home/longarm),
#      SRC_URL, DST_URL (default https://github.com/<repo>.git),
#      MIRROR_GH (tests: the gh binary, default gh). Needs git >= 2.38, jq.
set -euo pipefail
umask 077

die() {
	echo "::error::$*" >&2
	exit 1
}
src_repo=${SRC_REPO:-${GITHUB_REPOSITORY:-}}
dst_repo=${DST_REPO:-gim-home/longarm}
[[ -n $src_repo ]] || die "SRC_REPO (or GITHUB_REPOSITORY) is not set"
src_url=${SRC_URL:-https://github.com/$src_repo.git}
dst_url=${DST_URL:-https://github.com/$dst_repo.git}
gh_bin=${MIRROR_GH:-gh}
summary=${GITHUB_STEP_SUMMARY:-/dev/null}
footer="Mirrored from $src_repo#"

if [[ -z ${GIM_MIRROR_TOKEN:-} ]]; then
	echo "::notice title=mirror-gim::secret GIM_MIRROR_TOKEN is not set: nothing mirrored to $dst_repo (docs/ci-cd.md, Internal mirror)"
	echo "GIM_MIRROR_TOKEN is not set: nothing mirrored to $dst_repo." >>"$summary"
	exit 0
fi
command -v jq >/dev/null 2>&1 || die "jq is not installed"
command -v "$gh_bin" >/dev/null 2>&1 || die "gh is not installed"
jq() { command jq "$@" | tr -d '\r'; } # Windows' jq writes CRLF

# Credentials only through the environment: git asks this helper, which
# reads the token when it runs - never in argv, a URL or a config file.
export GIT_TERMINAL_PROMPT=0
# shellcheck disable=SC2016 # expanded by the helper's shell, not here
helper='!f() { test "$1" = get || return 0; echo username=x-access-token; echo "password=$MIRROR_GIT_TOKEN"; }; f'
git_as() { # git_as <token> <git args>: no other credential helper
	local tok=$1
	shift
	if [[ -n $tok ]]; then
		MIRROR_GIT_TOKEN=$tok GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0='' \
			GIT_CONFIG_KEY_1=credential.helper GIT_CONFIG_VALUE_1=$helper git "$@"
	else
		GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0='' git "$@"
	fi
}
src_git() { git_as "${SRC_TOKEN:-}" "$@"; }
dst_git() { git_as "$GIM_MIRROR_TOKEN" "$@"; }
src_gh() { GH_TOKEN=${SRC_TOKEN:-${GH_TOKEN:-}} "$gh_bin" "$@"; }
dst_gh() { GH_TOKEN=$GIM_MIRROR_TOKEN "$gh_bin" "$@"; }

work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/mirror-gim.XXXXXX")
trap 'rm -rf "$work"' EXIT
export GIT_DIR=$work/repo.git
git init -q --bare "$GIT_DIR"
failed=0
warn() {
	echo "::warning title=mirror-gim::$*"
	echo "- warning: $*" >>"$summary"
}
fail() {
	echo "::error title=mirror-gim::$*"
	echo "- error: $*" >>"$summary"
	failed=1
}

# --- pull requests on both sides --------------------------------------------
src_gh api --paginate "repos/$src_repo/pulls?state=open&per_page=100" | jq -s 'add // []' >"$work/src-prs.json"
dst_gh api --paginate "repos/$dst_repo/pulls?state=all&per_page=100" |
	jq -s --arg f "$footer" '(add // []) | map(. + {mirror_of: (
		(.body // "" | gsub("\r"; "") | split("\n") | map(select(startswith($f))) | last // "" | ltrimstr($f)) as $n
		| if ($n | test("^[0-9]+$")) then ($n | tonumber) else null end)})' >"$work/dst-prs.json"
# open fork PRs here: number per line
jq -r --arg r "$src_repo" '.[] | select(.head.repo.full_name != $r) | .number' "$work/src-prs.json" >"$work/fork-prs"
# the target's own open PRs: their heads are never pushed to or deleted
jq -r --arg r "$dst_repo" '.[] | select(.state == "open" and .mirror_of == null and .head.repo.full_name == $r) | .head.ref' \
	"$work/dst-prs.json" | sort -u >"$work/native-heads"
is_native_head() { grep -qxF -- "$1" "$work/native-heads"; }

# --- fetch -------------------------------------------------------------------
refspecs=("+refs/heads/*:refs/src/*")
while read -r n; do refspecs+=("+refs/pull/$n/head:refs/src-pr/$n"); done <"$work/fork-prs"
src_git fetch -q --no-tags "$src_url" "${refspecs[@]}"
git rev-parse -q --verify refs/src/main >/dev/null || die "$src_repo has no main branch"
dst_git ls-remote --heads "$dst_url" | sed 's|\trefs/heads/| |' >"$work/dst-heads" # <sha> <branch>
dst_tip() { awk -v b="$1" '$2 == b { print $1 }' "$work/dst-heads"; }
if [[ -n $(dst_tip main) ]]; then
	dst_git fetch -q --no-tags "$dst_url" "+refs/heads/main:refs/dst/main"
fi

# --- branches ----------------------------------------------------------------
: >"$work/wanted" # branches the mirror keeps there (main aside)
push=()
want() { # want <branch> <local ref>
	local b=$1 sha
	sha=$(git rev-parse "$2")
	echo "$b" >>"$work/wanted"
	if is_native_head "$b"; then
		warn "not pushing $b: it is the head of an open pull request of $dst_repo's own"
	elif [[ $(dst_tip "$b") != "$sha" ]]; then
		push+=("+$sha:refs/heads/$b")
	fi
}
while read -r ref; do
	b=${ref#refs/src/}
	[[ $b == main ]] || want "$b" "$ref"
done < <(git for-each-ref --format='%(refname)' refs/src/)
while read -r n; do
	# fork code: never run here, and kept away from the target's workflows
	# (a changed workflow there would run with the target's token)
	if ! mb=$(git merge-base refs/src/main "refs/src-pr/$n") ||
		[[ -n $(git diff --name-only "$mb" "refs/src-pr/$n" -- .github) ]]; then
		echo "::notice title=mirror-gim::fork pull request #$n changes .github/: not mirrored, review it here"
		continue
	fi
	want "pr-$n" "refs/src-pr/$n"
done <"$work/fork-prs"
if ((${#push[@]})); then
	if dst_git push -q --no-verify "$dst_url" "${push[@]}"; then
		echo "pushed ${#push[@]} branch(es)"
	else
		fail "pushing branches to $dst_repo failed (the token needs Contents and Workflows: write)"
	fi
fi

# --- main --------------------------------------------------------------------
s=$(git rev-parse refs/src/main)
d=$(git rev-parse -q --verify refs/dst/main || true)
main_msg="main up to date"
if [[ -n $d ]] && git merge-base --is-ancestor "$s" "$d"; then
	new="" # merged already
elif [[ -z $d ]] || git merge-base --is-ancestor "$d" "$s"; then
	new=$s # nothing of its own there (yet): a plain fast-forward
elif tree=$(git merge-tree --write-tree --no-messages "$d" "$s"); then
	new=$(GIT_AUTHOR_NAME=${MIRROR_GIT_NAME:-github-actions[bot]} GIT_AUTHOR_EMAIL=${MIRROR_GIT_EMAIL:-41898282+github-actions[bot]@users.noreply.github.com} \
		GIT_COMMITTER_NAME=${MIRROR_GIT_NAME:-github-actions[bot]} GIT_COMMITTER_EMAIL=${MIRROR_GIT_EMAIL:-41898282+github-actions[bot]@users.noreply.github.com} \
		git commit-tree "${tree%%$'\n'*}" -p "$d" -p "$s" -m "Sync main from $src_repo")
else
	new=""
	fail "main: merging $src_repo's main into $dst_repo's main conflicts; resolve it there by hand"
fi
if [[ -n $new ]]; then
	# never forced: if the target's main moved meanwhile, the next run merges again
	if dst_git push -q --no-verify "$dst_url" "$new:refs/heads/main"; then
		main_msg="main -> $(git rev-parse --short "$new")"
	else
		fail "pushing main to $dst_repo failed (moved meanwhile, or a ruleset the token's user can't bypass)"
	fi
fi
echo "$main_msg"

# --- pull requests -----------------------------------------------------------
pr_api() { # pr_api <method> <path> <json>: the API's response on stdout
	printf '%s' "$3" | dst_gh api -X "$1" "repos/$dst_repo/$2" --input -
}
pushed_or_there() { grep -qxF -- "$1" "$work/wanted" && ! is_native_head "$1"; }
prs_made=0 prs_updated=0 prs_closed=0
while read -r pr; do
	n=$(jq -r .number <<<"$pr")
	if [[ $(jq -r --arg r "$src_repo" '.head.repo.full_name == $r' <<<"$pr") == true ]]; then
		head=$(jq -r .head.ref <<<"$pr")
	else
		head=pr-$n
	fi
	pushed_or_there "$head" || continue # skipped above, with a warning
	base=$(jq -r .base.ref <<<"$pr")
	[[ $base == main ]] || grep -qxF -- "$base" "$work/wanted" || base=main
	want_pr=$(jq -c --arg f "$footer" --arg base "$base" '{title, base: $base,
		body: ((.body // "" | gsub("\r"; "") | rtrimstr("\n")) as $b | (if $b == "" then "" else $b + "\n\n" end) + $f + (.number | tostring))}' <<<"$pr")
	have=$(jq -c --argjson n "$n" '[.[] | select(.mirror_of == $n)] | (map(select(.state == "open")) + map(select(.state == "closed" and .merged_at == null))) | first // empty' "$work/dst-prs.json")
	if [[ -n $have ]]; then
		dn=$(jq -r .number <<<"$have")
		patch=$(jq -c --argjson h "$have" '. + {state: "open"} | with_entries(select(.value != (
			{title: $h.title, body: ($h.body // "" | gsub("\r"; "")), base: $h.base.ref, state: $h.state}[.key])))' <<<"$want_pr")
		if [[ $patch != "{}" ]]; then
			if pr_api PATCH "pulls/$dn" "$patch" >/dev/null; then
				prs_updated=$((prs_updated + 1))
				echo "#$n -> $dst_repo#$dn updated ($(jq -r 'keys | join(", ")' <<<"$patch"))"
				continue
			fi
			[[ $(jq -r .state <<<"$have") == open ]] && {
				fail "updating $dst_repo#$dn (#$n) failed"
				continue
			}
			# a closed one that can't be reopened (head re-pushed): a new one
		else
			continue
		fi
	fi
	create=$(jq -c --arg head "$head" --argjson d "$(jq .draft <<<"$pr")" '. + {head: $head, draft: ($d == true)}' <<<"$want_pr")
	if dn=$(pr_api POST pulls "$create" | jq -r .number); then
		prs_made=$((prs_made + 1))
		echo "#$n -> $dst_repo#$dn opened"
	else
		fail "opening the pull request for #$n in $dst_repo failed"
	fi
done < <(jq -c '.[]' "$work/src-prs.json")
# open mirrored PRs whose PR here is closed or merged
jq -r '.[] | .number' "$work/src-prs.json" | sort -u >"$work/src-open"
while read -r dn n; do
	grep -qxF -- "$n" "$work/src-open" && continue
	if pr_api PATCH "pulls/$dn" '{"state":"closed"}' >/dev/null; then
		prs_closed=$((prs_closed + 1))
		echo "$dst_repo#$dn closed (#$n is closed here)"
	else
		fail "closing $dst_repo#$dn failed"
	fi
done < <(jq -r '.[] | select(.state == "open" and .mirror_of != null) | "\(.number) \(.mirror_of)"' "$work/dst-prs.json")

# --- branches gone here ------------------------------------------------------
leases=() dels=()
while read -r sha b; do
	[[ $b == main || $b == dependabot/* ]] && continue
	grep -qxF -- "$b" "$work/wanted" && continue
	is_native_head "$b" && continue
	# only what the mirror pushed: its tip is a commit of this repository
	# (the API also knows commits no branch has any more)
	src_gh api --silent "repos/$src_repo/commits/$sha" </dev/null >/dev/null 2>&1 || continue
	leases+=("--force-with-lease=refs/heads/$b:$sha")
	dels+=(":refs/heads/$b")
done <"$work/dst-heads"
deleted=${#dels[@]}
if ((deleted)); then
	# the lease: not if it moved since it was listed
	if dst_git push -q --no-verify "${leases[@]}" "$dst_url" "${dels[@]}"; then
		printf 'deleted %s\n' "${dels[@]#:refs/heads/}"
	else
		fail "deleting branches in $dst_repo failed"
		deleted=0
	fi
fi

line="$dst_repo: ${#push[@]} branch(es) pushed, $deleted deleted; $main_msg; PRs: $prs_made opened, $prs_updated updated, $prs_closed closed"
echo "$line"
echo "$line" >>"$summary"
((failed == 0)) || exit 1
