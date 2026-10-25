#!/usr/bin/env bash
# privacy-check.sh: fails when the tracked tree contains details of a real
# deployment (hosts, addresses, accounts, keys) or other strings a
# maintainer wants kept out of the public tree. Run from anywhere in the
# repo; CI runs it on every push and pull request.
#
# The deny-list itself is private - publishing it would publish what it
# protects. It comes from
#   - $LONGARM_PRIVACY_PATTERNS (CI: the repository secret PRIVACY_PATTERNS), or
#   - the file .privacy-patterns in the repo root (gitignored),
# one pattern per line: an extended regex matched case-insensitively, or
# "w:<word>" for a whole-word match. Blank lines and "#" comments are ignored.
# Without a deny-list only the generic checks below run, unless
# LONGARM_PRIVACY_REQUIRED=1 (CI on pushes), which then fails.
#
# Examples in docs and tests use example.com, RFC 5737 addresses (192.0.2.x,
# 198.51.100.x, 203.0.113.x), 100.64.0.x for tailnet addresses and
# worker-1 / alice style names.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

fail=0
report() {
	echo "privacy-check: $1:" >&2
	echo "$2" >&2
	fail=1
}

patterns=${LONGARM_PRIVACY_PATTERNS:-}
if [[ -z $patterns && -f .privacy-patterns ]]; then
	patterns=$(<.privacy-patterns)
fi
if [[ -z $patterns ]]; then
	if [[ ${LONGARM_PRIVACY_REQUIRED:-} == 1 ]]; then
		echo "privacy-check: no deny-list (secret PRIVACY_PATTERNS / .privacy-patterns)" >&2
		exit 1
	fi
	echo "privacy-check: no deny-list, generic checks only"
else
	n=0
	while IFS= read -r p; do
		p=${p%$'\r'}
		[[ -z ${p//[[:space:]]/} || $p == \#* ]] && continue
		n=$((n + 1))
		if [[ $p == w:* ]]; then
			args=(-w -i -F -e "${p#w:}")
		else
			args=(-i -E -e "$p")
		fi
		# don't echo the pattern: CI logs are public
		if out=$(git grep -n -I "${args[@]}" -- . ':!.privacy-patterns'); then
			report "deny-list entry #$n" "$out"
		fi
	done <<<"$patterns"
	echo "privacy-check: $n deny-list entries checked"
fi

# The repository owner's account name only in the module path / repository
# URLs, the noreply e-mail address and the LICENSE.
owner=$(sed -n -E 's#^module github\.com/([^/]+)/.*#\1#p' go.mod)
if out=$(git grep -n -I -F "$owner" -- . ':!LICENSE' |
	sed -E "s#github\.com/$owner/longarm##g; s#$owner@users\.noreply\.github\.com##g" |
	grep -F "$owner"); then
	report "account name outside the module path / repository URLs" "$out"
fi

if [[ $fail -ne 0 ]]; then
	echo "privacy-check: FAILED" >&2
	exit 1
fi
echo "privacy-check: ok"
