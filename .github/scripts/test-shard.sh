#!/usr/bin/env bash
# test-shard.sh <package> <i> <n>
# Prints a go test -run pattern for shard i (1..n) of the package's top-level
# tests. Every test lands in exactly one shard; a new test needs no CI
# change. The tests are dealt out by duration (longest first, each to the
# shard with the least time so far) with the seconds in
# .github/test-durations.txt ("<test> <seconds>", from the timings artifacts
# of ci.yml's test jobs: .github/scripts/test-durations.sh); a test not
# listed there counts as the median.
set -euo pipefail
pkg=$1 i=$2 n=$3
durations=$(dirname "$0")/../test-durations.txt
names=$(go test -list . "$pkg" | tr -d '\r' | grep -E '^(Test|Example|Fuzz)' | LC_ALL=C sort)
pick=$(echo "$names" | awk -v i="$i" -v n="$n" -v f="$durations" '
	BEGIN {
		while ((getline l < f) > 0) {
			split(l, a, " ")
			if (a[1] ~ /^(Test|Example|Fuzz)/) { d[a[1]] = a[2] + 0; known[++k] = a[2] + 0 }
		}
		# the median of the known durations
		for (x = 1; x <= k; x++) for (y = x + 1; y <= k; y++) if (known[y] < known[x]) { t = known[x]; known[x] = known[y]; known[y] = t }
		med = k ? known[int((k + 1) / 2)] : 1
	}
	{ name[NR] = $1; w[NR] = ($1 in d) ? d[$1] : med }
	END {
		# longest first (ties: by name, the input order), each to the least
		# loaded shard (ties: the lowest number)
		for (x = 1; x <= NR; x++) ord[x] = x
		for (x = 1; x <= NR; x++) for (y = x + 1; y <= NR; y++)
			if (w[ord[y]] > w[ord[x]]) { t = ord[x]; ord[x] = ord[y]; ord[y] = t }
		for (s = 1; s <= n; s++) load[s] = 0
		for (x = 1; x <= NR; x++) {
			best = 1
			for (s = 2; s <= n; s++) if (load[s] < load[best]) best = s
			load[best] += w[ord[x]]
			if (best == i) print name[ord[x]]
		}
	}' | LC_ALL=C sort | paste -sd '|' -)
if [ -z "$pick" ]; then
	echo "test-shard.sh: shard $i of $n of $pkg is empty" >&2
	exit 1
fi
echo "^($pick)\$"
