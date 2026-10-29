#!/usr/bin/env bash
# test-durations.sh <run id>...
# Prints .github/test-durations.txt: the mean duration (seconds) of each of
# ./e2e's top-level tests over the timings artifacts (test-timings-*) of
# these ci.yml runs, for test-shard.sh's balancing:
#   .github/scripts/test-durations.sh 123 456 >.github/test-durations.txt
set -euo pipefail
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
for r in "$@"; do
	gh run download "$r" -p 'test-timings-*' -D "$dir/$r"
done
find "$dir" -name timings.txt -exec cat {} + | tr -d '\r' |
	awk '$1 ~ /\/e2e$/ { s[$2] += $3; c[$2]++ } END { for (t in s) printf "%s %.1f\n", t, s[t] / c[t] }' |
	LC_ALL=C sort
