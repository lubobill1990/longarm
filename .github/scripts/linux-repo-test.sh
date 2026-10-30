#!/usr/bin/env bash
# linux-repo-test.sh <site> <base url>
# Installs longarm from the repositories that `linux-repo.sh build <pkgdir> <site>
# <base url>` wrote, as docs/linux-repos.md tells users to, in throwaway
# containers (docker, --network host): Debian (apt, signed-by) and Fedora and
# AlmaLinux (dnf: gpgcheck, repo_gpgcheck). <site> is served on <base url>
# (http://127.0.0.1:<port>) for the test. Also checks that apt refuses the
# repository under another key and that the installed rpm carries the key's
# signature.
set -euo pipefail
site=${1:?usage: linux-repo-test.sh <site> <base url>}
base=${2:?usage: linux-repo-test.sh <site> <base url>}
port=${base##*:}
port=${port%%/*}
[[ $base == http://127.0.0.1:* && $port =~ ^[0-9]+$ ]] || {
	echo "base url: http://127.0.0.1:<port>" >&2
	exit 2
}
fpr=$(gpg --batch --show-keys --with-colons "$site/longarm.asc" | awk -F: '/^fpr:/ {print $10; exit}')
keyid=${fpr: -16}

python3 -m http.server --bind 127.0.0.1 --directory "$site" "$port" >/dev/null 2>&1 &
srv=$!
trap 'kill $srv 2>/dev/null || true' EXIT
for _ in $(seq 50); do
	curl -fsS -o /dev/null "$base/longarm.asc" 2>/dev/null && break
	sleep 0.2
done

echo "== Debian (apt, signed-by)"
docker run --rm --network host -e BASE="$base" -e FPR="$fpr" debian:stable-slim bash -euo pipefail -c '
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq
	apt-get install -y -qq curl gpg >/dev/null
	# the documented steps
	install -d -m 0755 /etc/apt/keyrings
	curl -fsSL "$BASE/longarm.gpg" -o /etc/apt/keyrings/longarm.gpg
	gpg --show-keys --with-colons /etc/apt/keyrings/longarm.gpg | grep -q "^fpr:.*:$FPR:"
	curl -fsSL "$BASE/longarm.sources" -o /etc/apt/sources.list.d/longarm.sources
	apt-get update 2>&1 | tee /tmp/update.log
	if grep -E "^(W|E|Err):" /tmp/update.log | grep -F "$BASE"; then echo "apt-get update: warnings/errors for the repository"; exit 1; fi
	apt-get install -y --no-install-recommends longarm
	longarm version
	dpkg -s longarm | grep -E "^(Version|Status):"
	# the same repository under another key must be refused
	mkdir /tmp/only
	sed "s#/etc/apt/keyrings/longarm.gpg#/usr/share/keyrings/debian-archive-keyring.gpg#" 		/etc/apt/sources.list.d/longarm.sources >/tmp/only/longarm.sources
	only=(-o Dir::Etc::SourceList=/dev/null -o Dir::Etc::SourceParts=/tmp/only -o Dir::State::Lists=/tmp/lists)
	mkdir -p /tmp/lists/partial
	if apt-get "${only[@]}" update >/tmp/bad.log 2>&1; then cat /tmp/bad.log; echo "apt accepted the repository under another key"; exit 1; fi
	grep -E "NO_PUBKEY|not signed" /tmp/bad.log
	echo "Debian: OK"
'

for image in fedora:latest almalinux:9; do
	echo "== $image (dnf, gpgcheck + repo_gpgcheck)"
	docker run --rm --network host -e BASE="$base" -e KEYID="$keyid" "$image" bash -euo pipefail -c '
		curl -fsSL -o /etc/yum.repos.d/longarm.repo "$BASE/longarm.repo"
		cat /etc/yum.repos.d/longarm.repo
		grep -qx gpgcheck=1 /etc/yum.repos.d/longarm.repo
		grep -qx repo_gpgcheck=1 /etc/yum.repos.d/longarm.repo
		dnf -y --repo=longarm install longarm
		longarm version
		sig=$(rpm -q --qf "%{RSAHEADER:pgpsig}\n" longarm)
		echo "signature: $sig"
		echo "$sig" | grep -qi "$KEYID"
		echo "OK"
	'
done
