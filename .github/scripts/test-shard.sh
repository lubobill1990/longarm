#!/usr/bin/env bash
# test-shard.sh <package> <i> <n>
# Prints a go test -run pattern for shard i (1..n) of the package's top-level
# tests: the names from go test -list, sorted, dealt out round-robin. Every
# test lands in exactly one shard; a new test needs no CI change.
set -euo pipefail
pkg=$1 i=$2 n=$3
names=$(go test -list . "$pkg" | tr -d '\r' | grep -E '^(Test|Example|Fuzz)' | LC_ALL=C sort)
pick=$(echo "$names" | awk -v i="$i" -v n="$n" '(NR - 1) % n + 1 == i' | paste -sd '|' -)
if [ -z "$pick" ]; then
	echo "test-shard.sh: shard $i of $n of $pkg is empty" >&2
	exit 1
fi
echo "^($pick)\$"
