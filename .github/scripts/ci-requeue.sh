#!/usr/bin/env bash
# ci-requeue.sh <run id> <attempt>
# For ci-requeue.yml: when attempt <attempt> of ci.yml's run failed only
# because GitHub never started some jobs (no runner acquired: hosted macOS
# capacity; they end "cancelled" without a runner), re-run them - and with
# them the required "tests" check - up to MAX_ATTEMPTS attempts in all. A
# job that ran and failed is never re-run: flaky tests stay visible.
# DRY_RUN=1 only reports.
set -euo pipefail
run=$1 attempt=$2
max=${MAX_ATTEMPTS:-3}
repo=${GITHUB_REPOSITORY:?}
summary=${GITHUB_STEP_SUMMARY:-/dev/stdout}
# name<TAB>conclusion<TAB>runner (empty: never started)
jobs=$(gh api --paginate "repos/$repo/actions/runs/$run/attempts/$attempt/jobs?per_page=100" \
	-q '.jobs[] | [.name, (.conclusion // "none"), (.runner_name // "")] | @tsv')
unstarted=$(awk -F'\t' '$2 == "cancelled" && $3 == "" { print $1 }' <<<"$jobs")
failed=$(awk -F'\t' '$1 != "tests" && ($2 == "failure" || $2 == "timed_out" || ($2 == "cancelled" && $3 != "")) { print $1 }' <<<"$jobs")
{
	echo "### ci run $run, attempt $attempt"
	echo
	if [ -z "$unstarted" ]; then
		echo "No job was left unstarted: nothing to re-queue."
	elif [ -n "$failed" ]; then
		echo "Jobs failed for real (not re-run, so the failure stays visible):"
		sed 's/^/- /' <<<"$failed"
	elif [ "$attempt" -ge "$max" ]; then
		echo "GitHub did not start these jobs, but the run had $attempt attempts already:"
		sed 's/^/- /' <<<"$unstarted"
	else
		echo "GitHub did not start these jobs (no hosted runner acquired); re-running them:"
		sed 's/^/- /' <<<"$unstarted"
	fi
} >>"$summary"
if [ -n "$unstarted" ] && [ -z "$failed" ] && [ "$attempt" -lt "$max" ]; then
	echo "::warning::re-queueing $(wc -l <<<"$unstarted") job(s) GitHub never started in ci run $run"
	if [ -z "${DRY_RUN:-}" ]; then
		gh run rerun "$run" --failed --repo "$repo"
	fi
fi
