#!/usr/bin/env bash
# linux-repo.sh: the signed apt and yum repositories (GitHub Pages; docs/linux-repos.md).
#
#   linux-repo.sh key <public key>
#       imports the secret $LONGARM_GPG_PRIVATE_KEY (passphrase: $LONGARM_GPG_PASSPHRASE,
#       optional) into $GNUPGHOME and checks that it is the key of <public key> (the
#       committed packaging/linux/repo/longarm.asc): a wrong secret fails here, before
#       anything is signed with it
#   linux-repo.sh previous <base url> <public key> <pkgdir> <keep> <version>
#       the packages of the newest <keep>-1 versions other than <version> from the
#       repositories published at <base url>, verified as apt and dnf would (InRelease and
#       repomd.xml signed by <public key>, hashes): what was vetted when it was first
#       published, never re-taken from (mutable) GitHub Release assets; none yet: nothing
#   linux-repo.sh build <pkgdir> <site> <base url> <public key>
#       <site>/deb (apt: pool/, dists/stable/ with Release, InRelease, Release.gpg),
#       <site>/rpm (yum/dnf: packages/, repodata/ with repomd.xml.asc), the public key
#       (longarm.asc, longarm.gpg), longarm.repo, longarm.sources, index.html.
#       Signs with the key of <public key> from $GNUPGHOME (`key` above); rpms that
#       are not signed by that key (releases before the rpm signing) are left out.
#
# Needs: gpg, gpgv, curl, dpkg-deb, apt-ftparchive (apt-utils), rpm, createrepo_c.
set -euo pipefail

die() {
	echo "::error::$*" >&2
	exit 1
}

fpr_of() { # <public key file> -> its primary key's fingerprint
	gpg --batch --show-keys --with-colons "$1" | awk -F: '/^fpr:/ {print $10; exit}'
}

passfile() { echo "${GNUPGHOME:?GNUPGHOME is not set}/longarm-passphrase"; }

gpgsign() { # <fingerprint> <gpg args...>
	local fpr=$1
	shift
	gpg --batch --yes --pinentry-mode loopback --passphrase-file "$(passfile)" \
		--local-user "$fpr!" --digest-algo SHA512 "$@"
}

cmd_key() {
	local pub=${1:?usage: linux-repo.sh key <public key>}
	[[ -n ${LONGARM_GPG_PRIVATE_KEY:-} ]] || die "LONGARM_GPG_PRIVATE_KEY is empty"
	: "${GNUPGHOME:?GNUPGHOME is not set}"
	mkdir -p "$GNUPGHOME"
	chmod 0700 "$GNUPGHOME"
	(
		umask 077
		printf '%s' "${LONGARM_GPG_PASSPHRASE:-}" >"$(passfile)"
	)
	printf '%s\n' "$LONGARM_GPG_PRIVATE_KEY" | gpg --batch --quiet --import 2>/dev/null ||
		die "LONGARM_GPG_PRIVATE_KEY: gpg --import failed"
	local want
	want=$(fpr_of "$pub")
	[[ -n $want ]] || die "$pub: no public key"
	gpg --batch --with-colons --list-secret-keys "$want" 2>/dev/null | grep -q "^fpr:.*:$want:" ||
		die "LONGARM_GPG_PRIVATE_KEY is not the key of $pub ($want); rotating the key: docs/linux-repos.md"
	# the passphrase opens it, and the signature verifies against the committed key
	local t
	t=$(mktemp -d)
	echo probe >"$t/probe"
	gpgsign "$want" --detach-sign -o "$t/probe.sig" "$t/probe" 2>/dev/null ||
		die "signing with $want failed (LONGARM_GPG_PASSPHRASE?)"
	gpg --batch --yes --dearmor -o "$t/pub.gpg" <"$pub"
	gpgv --keyring "$t/pub.gpg" "$t/probe.sig" "$t/probe" 2>/dev/null || die "probe signature does not verify"
	rm -rf "$t"
	echo "signing key $want"
}

cmd_previous() {
	local base=${1:?usage: linux-repo.sh previous <base url> <public key> <pkgdir> <keep> <version>}
	local pub=${2:?} dir=${3:?} keep=${4:?} cur=${5:?}
	base=${base%/}
	[[ $keep =~ ^[1-9][0-9]*$ ]] || die "keep: '$keep' is not a positive number"
	mkdir -p "$dir"
	local t
	t=$(mktemp -d)
	gpg --batch --yes --dearmor -o "$t/pub.gpg" <"$pub"
	local code
	code=$(curl -sS -o "$t/InRelease" -w '%{http_code}' "$base/deb/dists/stable/InRelease") || code=000
	if [[ $code == 404 ]]; then
		echo "::notice::$base: no repository yet, starting with this release only"
		rm -rf "$t"
		return 0
	fi
	[[ $code == 200 ]] || die "$base/deb/dists/stable/InRelease: HTTP $code"
	# the published repository, verified as apt would: InRelease by the key,
	# the Packages indexes by InRelease's hashes, the .debs by the indexes'
	gpgv --keyring "$t/pub.gpg" --output "$t/Release" "$t/InRelease" 2>/dev/null ||
		die "$base: InRelease is not signed by the key of $pub"
	local a
	: >"$t/all"
	for a in amd64 arm64; do
		local want got
		want=$(awk -v f="main/binary-$a/Packages" '/^SHA256:/ {s=1; next} /^[^ ]/ {s=0} s && $3 == f {print $1}' "$t/Release")
		[[ $want =~ ^[0-9a-f]{64}$ ]] || die "Release: no SHA256 of main/binary-$a/Packages"
		curl -fsSL -o "$t/Packages" "$base/deb/dists/stable/main/binary-$a/Packages"
		got=$(sha256sum "$t/Packages" | cut -d' ' -f1)
		[[ $got == "$want" ]] || die "main/binary-$a/Packages: sha256 mismatch"
		# package version filename sha256
		awk '/^Package: / {p=$2} /^Version: / {v=$2} /^Filename: / {f=$2} /^SHA256: / {h=$2}
			/^$/ {if (p != "") print p, v, f, h; p=v=f=h=""}
			END {if (p != "") print p, v, f, h}' "$t/Packages" >>"$t/all"
	done
	# the newest <keep>-1 versions of longarm other than <version>
	local vers
	vers=$(awk -v c="$cur" '$1 == "longarm" && $2 != c {print $2}' "$t/all" | sort -uV | tail -n $((keep - 1)))
	if [[ -z $vers ]]; then
		echo "previous versions: none"
		rm -rf "$t"
		return 0
	fi
	echo "previous versions: $(echo "$vers" | tr '\n' ' ')"
	mkdir -p "$dir/deb" "$dir/rpm"
	local v f h
	while read -r _ v f h; do
		grep -qxF -- "$v" <<<"$vers" || continue
		[[ $f =~ ^pool/main/[a-z0-9]/[a-z0-9.+-]+/[A-Za-z0-9._+~-]+\.deb$ ]] || die "Packages: odd Filename '$f'"
		curl -fsSL -o "$dir/deb/${f##*/}" "$base/deb/$f"
		[[ $(sha256sum "$dir/deb/${f##*/}" | cut -d' ' -f1) == "$h" ]] || die "$f: sha256 mismatch"
	done <"$t/all"
	# rpms: by version from the (signed) repomd's primary; each rpm's own
	# signature is checked by `build`
	curl -fsSL -o "$t/repomd.xml" "$base/rpm/repodata/repomd.xml"
	curl -fsSL -o "$t/repomd.xml.asc" "$base/rpm/repodata/repomd.xml.asc"
	gpgv --keyring "$t/pub.gpg" "$t/repomd.xml.asc" "$t/repomd.xml" 2>/dev/null ||
		die "$base: repomd.xml is not signed by the key of $pub"
	local prim
	prim=$(grep -o 'href="repodata/[A-Za-z0-9._-]*primary\.xml\.gz"' "$t/repomd.xml" | head -1 | cut -d'"' -f2)
	[[ -n $prim ]] || die "repomd.xml: no primary"
	curl -fsSL -o "$t/primary.xml.gz" "$base/rpm/$prim"
	local href
	for href in $(gzip -dc "$t/primary.xml.gz" | grep -o 'href="packages/[A-Za-z0-9._+~^-]*\.rpm"' | cut -d'"' -f2); do
		# packages/<name>-<version>-<release>.<arch>.rpm
		v=$(sed -E 's#^packages/longarm-([^-]+)-[^-]+\.[a-z0-9_]+\.rpm$#\1#' <<<"$href")
		# (an rpm version has "_" where the deb's has "-": prereleases, snapshots)
		if [[ $v == "$href" ]] || ! tr - _ <<<"$vers" | grep -qxF -- "${v//-/_}"; then continue; fi
		curl -fsSL -o "$dir/rpm/${href#packages/}" "$base/rpm/$href"
	done
	rm -rf "$t"
	echo "previous: $(find "$dir" -type f | wc -l) packages"
}

write_index() { # <site> <base> <fpr>
	local site=$1 base=$2 fpr=$3
	cat >"$site/index.html" <<EOF
<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Longarm package repositories</title>
<style>body{font-family:system-ui,sans-serif;max-width:52rem;margin:2rem auto;padding:0 1rem;line-height:1.5}pre{background:#f4f4f4;padding:.8rem;overflow-x:auto}</style>
</head><body>
<h1>Longarm package repositories</h1>
<p>Signed apt and dnf/yum repositories for <a href="https://github.com/lubobill1990/longarm">Longarm</a>
(packages <code>longarm</code> and <code>longarm-hub</code>; amd64/x86_64 and arm64/aarch64).</p>
<p>Signing key: <a href="longarm.asc">longarm.asc</a> (binary keyring: <a href="longarm.gpg">longarm.gpg</a>),
"Longarm Release Signing", fingerprint <code>$fpr</code>.
Check it with <code>gpg --show-keys longarm.asc</code>.</p>
<h2>Debian, Ubuntu</h2>
<pre>sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL $base/longarm.gpg | sudo tee /etc/apt/keyrings/longarm.gpg &gt;/dev/null
curl -fsSL $base/longarm.sources | sudo tee /etc/apt/sources.list.d/longarm.sources &gt;/dev/null
sudo apt-get update &amp;&amp; sudo apt-get install longarm</pre>
<h2>Fedora, RHEL, Rocky, Alma</h2>
<pre>sudo curl -fsSL -o /etc/yum.repos.d/longarm.repo $base/longarm.repo
sudo dnf install longarm</pre>
<p>Updates come with <code>apt upgrade</code> / <code>dnf upgrade</code>.
More: <a href="https://github.com/lubobill1990/longarm/blob/main/docs/linux-repos.md">docs/linux-repos.md</a>.</p>
</body></html>
EOF
}

cmd_build() {
	local pkg=${1:?usage: linux-repo.sh build <pkgdir> <site> <base url> <public key>}
	local site=${2:?} base=${3:?} pub=${4:?}
	base=${base%/}
	local fpr
	fpr=$(fpr_of "$pub")
	[[ -n $fpr ]] || die "$pub: no public key"
	gpg --batch --with-colons --list-secret-keys "$fpr" >/dev/null 2>&1 ||
		die "the signing key $fpr is not in GNUPGHOME (linux-repo.sh key)"
	rm -rf "$site"
	mkdir -p "$site/deb" "$site/rpm/packages"
	site=$(cd "$site" && pwd)
	cp "$pub" "$site/longarm.asc"
	gpg --batch --yes --dearmor -o "$site/longarm.gpg" <"$pub"

	# apt: pool/main/<l>/<package>/<package>_<version>_<arch>.deb
	local f n=0
	while IFS= read -r -d '' f; do
		local name ver arch
		name=$(dpkg-deb -f "$f" Package)
		ver=$(dpkg-deb -f "$f" Version)
		arch=$(dpkg-deb -f "$f" Architecture)
		[[ $name =~ ^[a-z0-9][a-z0-9.+-]*$ && $ver =~ ^[0-9A-Za-z.+~:-]+$ ]] || die "$f: odd package name/version"
		case $arch in amd64 | arm64) ;; *)
			echo "skipping $f ($arch)"
			continue
			;;
		esac
		mkdir -p "$site/deb/pool/main/${name:0:1}/$name"
		cp "$f" "$site/deb/pool/main/${name:0:1}/$name/${name}_${ver}_${arch}.deb"
		n=$((n + 1))
	done < <(find "$pkg" -type f -name '*.deb' -print0)
	((n > 0)) || die "$pkg: no .deb"
	(
		cd "$site/deb"
		local a
		for a in amd64 arm64; do
			mkdir -p "dists/stable/main/binary-$a"
			apt-ftparchive --arch "$a" packages pool >"dists/stable/main/binary-$a/Packages"
			gzip -9nk "dists/stable/main/binary-$a/Packages"
		done
		apt-ftparchive \
			-o APT::FTPArchive::Release::Origin=Longarm \
			-o APT::FTPArchive::Release::Label=Longarm \
			-o APT::FTPArchive::Release::Suite=stable \
			-o APT::FTPArchive::Release::Codename=stable \
			-o APT::FTPArchive::Release::Architectures="amd64 arm64" \
			-o APT::FTPArchive::Release::Components=main \
			-o APT::FTPArchive::Release::Description="Longarm (https://github.com/lubobill1990/longarm)" \
			release dists/stable >"$site/Release.tmp"
		mv "$site/Release.tmp" dists/stable/Release
		gpgsign "$fpr" --clearsign -o dists/stable/InRelease dists/stable/Release
		gpgsign "$fpr" --armor --detach-sign -o dists/stable/Release.gpg dists/stable/Release
		gpgv --keyring "$site/longarm.gpg" dists/stable/InRelease
		gpgv --keyring "$site/longarm.gpg" dists/stable/Release.gpg dists/stable/Release
	)
	echo "apt: $n packages"

	# yum/dnf: only rpms signed by the key (gpgcheck=1)
	local db
	db=$(mktemp -d)
	rpmkeys --dbpath "$db" --import "$pub"
	n=0
	while IFS= read -r -d '' f; do
		if ! rpmkeys --dbpath "$db" --checksig "$f" 2>/dev/null | grep -q 'signatures OK'; then
			echo "::notice::$f is not signed by $fpr: not in the yum repository"
			continue
		fi
		local nevra
		nevra=$(rpm --dbpath "$db" -qp --qf '%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}' "$f" 2>/dev/null)
		[[ $nevra =~ ^[A-Za-z0-9_._+~^-]+$ ]] || die "$f: odd NEVRA '$nevra'"
		cp "$f" "$site/rpm/packages/$nevra.rpm"
		n=$((n + 1))
	done < <(find "$pkg" -type f -name '*.rpm' -print0)
	rm -rf "$db"
	if ((n == 0)); then
		echo "::warning::no rpm signed by $fpr: the yum repository is empty"
	fi
	# gzip metadata, no sqlite: readable by the dnf/yum of RHEL 8 and later
	createrepo_c --quiet --no-database --general-compress-type=gz "$site/rpm"
	gpgsign "$fpr" --armor --detach-sign -o "$site/rpm/repodata/repomd.xml.asc" "$site/rpm/repodata/repomd.xml"
	gpgv --keyring "$site/longarm.gpg" "$site/rpm/repodata/repomd.xml.asc" "$site/rpm/repodata/repomd.xml"
	echo "yum: $n packages"

	cat >"$site/longarm.sources" <<EOF
Types: deb
URIs: $base/deb
Suites: stable
Components: main
Architectures: amd64 arm64
Signed-By: /etc/apt/keyrings/longarm.gpg
EOF
	cat >"$site/longarm.repo" <<EOF
[longarm]
name=Longarm
baseurl=$base/rpm
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$base/longarm.asc
EOF
	write_index "$site" "$base" "$fpr"
	du -sh "$site"
}

case ${1:-} in
key | previous | build)
	c=$1
	shift
	"cmd_$c" "$@"
	;;
*)
	sed -n '2,20p' "$0" >&2
	exit 2
	;;
esac
