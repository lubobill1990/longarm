#!/usr/bin/env bash
# linux-repo.sh: the signed apt and yum repositories (GitHub Pages; docs/linux-repos.md).
#
#   linux-repo.sh key <public key>
#       imports the secret $LONGARM_GPG_PRIVATE_KEY (passphrase: $LONGARM_GPG_PASSPHRASE,
#       optional) into $GNUPGHOME and checks that it is the key of <public key> (the
#       committed packaging/linux/repo/longarm.asc): a wrong secret fails here, before
#       anything is signed with it
#   linux-repo.sh fetch <owner/repo> <keep> <pkgdir> [<tag>]
#       the deb/rpm packages of the newest <keep> stable GitHub Releases (gh, GH_TOKEN);
#       <tag> must be one of them (the release that runs this)
#   linux-repo.sh build <pkgdir> <site> <base url> <public key>
#       <site>/deb (apt: pool/, dists/stable/ with Release, InRelease, Release.gpg),
#       <site>/rpm (yum/dnf: packages/, repodata/ with repomd.xml.asc), the public key
#       (longarm.asc, longarm.gpg), longarm.repo, longarm.sources, index.html.
#       Signs with the key of <public key> from $GNUPGHOME (`key` above); rpms that
#       are not signed by that key (releases before the rpm signing) are left out.
#
# Needs: gpg, gpgv, dpkg-deb, apt-ftparchive (apt-utils), rpm, createrepo_c.
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

cmd_fetch() {
	local repo=${1:?usage: linux-repo.sh fetch <owner/repo> <keep> <pkgdir> [<tag>]}
	local keep=${2:?} dir=${3:?} need=${4:-}
	[[ $keep =~ ^[1-9][0-9]*$ ]] || die "keep: '$keep' is not a positive number"
	mkdir -p "$dir"
	local tags
	tags=$(gh release list --repo "$repo" --exclude-drafts --exclude-pre-releases --limit 200 \
		--json tagName --jq '.[].tagName' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n "$keep")
	[[ -n $tags ]] || die "$repo: no stable release"
	if [[ -n $need ]] && ! grep -qxF -- "$need" <<<"$tags"; then
		die "$need is not among the newest $keep stable releases: $(echo "$tags" | tr '\n' ' ')"
	fi
	local tag
	for tag in $tags; do
		mkdir -p "$dir/$tag"
		# longarm-linux-<arch>.deb|rpm (the hub's names) and longarm-hub_<v>_<arch>.deb
		gh release download "$tag" --repo "$repo" --dir "$dir/$tag" --clobber \
			--pattern 'longarm-linux-*.deb' --pattern 'longarm-linux-*.rpm' --pattern 'longarm-hub_*.deb'
		# the files the release lists in SHA256SUMS must match it
		if gh release download "$tag" --repo "$repo" --dir "$dir/$tag" --clobber --pattern SHA256SUMS 2>/dev/null; then
			(cd "$dir/$tag" && grep -E ' (longarm-linux-[a-z0-9]+\.(deb|rpm)|longarm-hub_[^ ]+\.deb)$' SHA256SUMS | sha256sum -c --quiet -) ||
				die "$tag: packages don't match the release's SHA256SUMS"
			rm "$dir/$tag/SHA256SUMS"
		fi
		echo "$tag: $(find "$dir/$tag" -type f | wc -l) packages"
	done
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
key | fetch | build)
	c=$1
	shift
	"cmd_$c" "$@"
	;;
*)
	sed -n '2,20p' "$0" >&2
	exit 2
	;;
esac
