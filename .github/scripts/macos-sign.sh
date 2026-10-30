#!/usr/bin/env bash
# macos-sign.sh <dir> <version>
# release.yml's build-macos job (macOS runner): turns the darwin files of the
# build-unix artifact (<dir>: hub/, extra/, checksums/unix.txt) into the
# shipped ones, in place, before publish checksums and minisigns anything:
#
#   1. signs the darwin binaries in hub/ (longarm, longarmd, longarm-hub,
#      longarm-relay; amd64, arm64) with the Developer ID Application
#      certificate: hardened runtime, secure timestamp, identifier
#      com.longarm.<tool>. Self-update downloads exactly these files.
#   2. repacks hub/longarm-darwin-<arch>.tar.gz with the signed longarm and
#      longarmd (after checking the archive held the same unsigned binaries)
#      and updates their lines in checksums/unix.txt;
#   3. builds hub/longarm-darwin-<arch>.pkg (packaging/macos/build-pkg.sh),
#      signed with the Developer ID Installer certificate;
#   4. notarizes the binaries (one zip) and each pkg with notarytool (App
#      Store Connect API key), staples the tickets to the pkgs and checks
#      them with spctl.
#
# Env (all optional; whatever is missing is skipped with a warning, the
# files then ship unsigned / not notarized; the pkgs are always built):
#   APPLE_DEVELOPER_ID_APP_P12, _PASSWORD          base64 of the .p12, its password
#   APPLE_DEVELOPER_ID_INSTALLER_P12, _PASSWORD    base64 of the .p12, its password
#   APPLE_NOTARY_API_KEY_P8, APPLE_NOTARY_KEY_ID, APPLE_NOTARY_ISSUER_ID
#   APPLE_TEAM_ID         the certificates must belong to this team (checked)
#   LONGARM_MACOS_UNSIGNED_REASON   the warning's reason when nothing is set
# Secrets are written to a private temp dir (umask 077) and deleted with the
# temporary keychains when the script exits. Never run with -x.
# Runs with macOS's bash 3.2.
set -euo pipefail
umask 077

die() {
	echo "::error::macos-sign: $*" >&2
	exit 1
}
warn() { echo "::warning title=macOS signing::$*"; }

[[ $# -eq 2 ]] || die "usage: macos-sign.sh <dir> <version>"
dir=$1 v=$2
hub=$dir/hub
sums=$dir/checksums/unix.txt
done_marker=$dir/checksums/macos.done
repo=$(cd "$(dirname "$0")/../.." && pwd)
[[ -d $hub && -f $sums ]] || die "$dir is not build-unix's artifact (hub/, checksums/unix.txt)"
if [[ -f $done_marker ]]; then
	echo "macos-sign: $dir was already processed ($(cat "$done_marker")); nothing to do"
	exit 0
fi
command -v codesign >/dev/null || die "needs macOS (codesign, pkgbuild, productbuild, notarytool)"

archs="amd64 arm64"
tools="longarm longarmd longarm-hub longarm-relay"
for a in $archs; do
	for t in longarm longarmd; do
		[[ -f $hub/$t-darwin-$a ]] || die "missing hub/$t-darwin-$a"
	done
	[[ -f $hub/longarm-darwin-$a.tar.gz ]] || die "missing hub/longarm-darwin-$a.tar.gz"
	[[ ! -e $hub/longarm-darwin-$a.pkg ]] || die "hub/longarm-darwin-$a.pkg exists already"
done

# ---- what is configured -------------------------------------------------------
app=0 inst=0 notary=0
[[ -n ${APPLE_DEVELOPER_ID_APP_P12:-} ]] && app=1
[[ -n ${APPLE_DEVELOPER_ID_INSTALLER_P12:-} ]] && inst=1
n=0
for x in "${APPLE_NOTARY_API_KEY_P8:-}" "${APPLE_NOTARY_KEY_ID:-}" "${APPLE_NOTARY_ISSUER_ID:-}"; do
	[[ -n $x ]] && n=$((n + 1))
done
case $n in
0) ;;
3) notary=1 ;;
*) die "APPLE_NOTARY_API_KEY_P8, APPLE_NOTARY_KEY_ID and APPLE_NOTARY_ISSUER_ID go together: set all three or none" ;;
esac
team=${APPLE_TEAM_ID:-}
if [[ -n $team && ! $team =~ ^[A-Z0-9]{10}$ ]]; then
	die "APPLE_TEAM_ID must be the 10-character team ID (Membership details in the Apple Developer account)"
fi
if [[ $app == 0 && $inst == 0 && $notary == 0 ]]; then
	warn "${LONGARM_MACOS_UNSIGNED_REASON:-the Apple secrets are not set} - the macOS binaries, archives and .pkg installers are NOT signed or notarized; MDM-managed Macs refuse the .pkg and Gatekeeper blocks downloaded binaries (docs/ci-cd.md 2.8)"
else
	[[ $app == 1 ]] || warn "APPLE_DEVELOPER_ID_APP_P12 is not set: the macOS binaries are not signed"
	[[ $inst == 1 ]] || warn "APPLE_DEVELOPER_ID_INSTALLER_P12 is not set: the .pkg installers are not signed"
	[[ $notary == 1 ]] || warn "APPLE_NOTARY_API_KEY_P8 / _KEY_ID / _ISSUER_ID are not set: nothing is notarized"
	[[ $notary == 0 || $app == 1 ]] || warn "notarization needs signed binaries (APPLE_DEVELOPER_ID_APP_P12): skipped"
	[[ $app == 0 || -n $team ]] || warn "variable APPLE_TEAM_ID is not set: the certificates' team is not checked"
fi

# ---- private temp dir, keychains ---------------------------------------------
sec=$(mktemp -d "${RUNNER_TEMP:-/tmp}/macos-sign.XXXXXX")
keychains=()
orig_list=()
while IFS= read -r k; do
	k=${k#"${k%%[![:space:]]*}"}
	k=${k#\"}
	k=${k%\"}
	[[ -n $k ]] && orig_list+=("$k")
done < <(security list-keychains -d user)
cleanup() {
	set +e
	if [[ ${#keychains[@]} -gt 0 ]]; then
		if [[ ${#orig_list[@]} -gt 0 ]]; then
			security list-keychains -d user -s "${orig_list[@]}"
		fi
		for k in "${keychains[@]}"; do security delete-keychain "$k" 2>/dev/null; done
	fi
	rm -rf "$sec"
}
trap cleanup EXIT

# Apple's Developer ID intermediate CAs (committed, pinned): codesign embeds
# the chain, notarization requires it, a .p12 usually holds only the leaf.
cas="DeveloperIDG2CA.cer:f16cd3c54c7f83cea4bf1a3e6a0819c8aaa8e4a1528fd144715f350643d2df3a
DeveloperIDCA.cer:7afc9d01a62f03a2de9637936d4afe68090d2de18d03f29c88cfb0b1ba63587f"

# import_identity <name> <base64 p12> <password>: a new keychain with the
# identity (and the intermediates); sets ID_HASH, ID_NAME, ID_KC.
import_identity() {
	local name=$1 b64=$2 pw=$3 kc kcpw out c f want
	kc=$sec/$name.keychain-db
	kcpw=$(openssl rand -hex 24)
	security create-keychain -p "$kcpw" "$kc"
	keychains+=("$kc")
	security set-keychain-settings -lut 7200 "$kc"
	security unlock-keychain -p "$kcpw" "$kc"
	if ! printf '%s' "$b64" | tr -d ' \t\r\n' | openssl base64 -d -A >"$sec/$name.p12" || [[ ! -s $sec/$name.p12 ]]; then
		die "$name: the .p12 secret is not base64"
	fi
	if ! security import "$sec/$name.p12" -k "$kc" -f pkcs12 -P "$pw" \
		-T /usr/bin/codesign -T /usr/bin/productbuild -T /usr/bin/productsign -T /usr/bin/pkgbuild >/dev/null; then
		die "$name: security import failed (wrong password, or not a .p12 with the private key)"
	fi
	rm -f "$sec/$name.p12"
	security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$kcpw" "$kc" >/dev/null
	out=$(security find-identity -p basic "$kc" | sed -n '/Matching identities/,/identities found/p')
	if [[ $(grep -cE '^ *[0-9]+\) [0-9A-F]{40} ' <<<"$out") -ne 1 ]]; then
		die "$name: the .p12 must hold exactly one identity (certificate + private key)"
	fi
	ID_HASH=$(sed -nE 's/^ *[0-9]+\) ([0-9A-F]{40}) .*/\1/p' <<<"$out")
	ID_NAME=$(sed -nE 's/^ *[0-9]+\) [0-9A-F]{40} "(.*)".*/\1/p' <<<"$out")
	ID_KC=$kc
	for c in $cas; do
		f=$repo/packaging/macos/certs/${c%%:*} want=${c#*:}
		[[ $(shasum -a 256 "$f" | cut -d' ' -f1) == "$want" ]] || die "$f: sha256 mismatch"
		security import "$f" -k "$kc" >/dev/null 2>&1 || true # already there: fine
	done
	echo "$name: identity \"$ID_NAME\" ($ID_HASH)"
	if [[ -n $team && $ID_NAME != *"($team)" ]]; then
		die "$name: \"$ID_NAME\" is not a certificate of team $team (APPLE_TEAM_ID)"
	fi
}

if [[ $app == 1 ]]; then
	import_identity app "$APPLE_DEVELOPER_ID_APP_P12" "${APPLE_DEVELOPER_ID_APP_P12_PASSWORD:-}"
	app_hash=$ID_HASH app_kc=$ID_KC
fi
if [[ $inst == 1 ]]; then
	import_identity installer "$APPLE_DEVELOPER_ID_INSTALLER_P12" "${APPLE_DEVELOPER_ID_INSTALLER_P12_PASSWORD:-}"
	inst_name=$ID_NAME inst_kc=$ID_KC
fi
if [[ ${#keychains[@]} -gt 0 ]]; then
	# codesign and productbuild build the certificate chain from the search list
	security list-keychains -d user -s "${keychains[@]}" ${orig_list[@]+"${orig_list[@]}"}
fi

sha() { shasum -a 256 "$1" | cut -d' ' -f1; }

# ---- 1. sign the binaries ----------------------------------------------------
mkdir "$sec/unsigned"
signed_bins=()
for a in $archs; do
	for t in $tools; do
		f=$hub/$t-darwin-$a
		[[ -f $f ]] || continue
		cp "$f" "$sec/unsigned/$t-darwin-$a"
		signed_bins+=("$f")
		[[ $app == 1 ]] || continue
		codesign --force --options runtime --timestamp --identifier "com.longarm.$t" \
			--sign "$app_hash" --keychain "$app_kc" "$f"
		codesign --verify --strict --verbose=2 "$f"
		info=$(codesign -dv --verbose=2 "$f" 2>&1)
		grep -q '^CodeDirectory .*flags=.*(runtime)' <<<"$info" || die "$f: no hardened runtime"
		grep -q "^Identifier=com.longarm.$t\$" <<<"$info" || die "$f: wrong identifier"
		grep -q '^Timestamp=' <<<"$info" || die "$f: no secure timestamp"
		if [[ -n $team ]]; then
			grep -q "^TeamIdentifier=$team\$" <<<"$info" || die "$f: TeamIdentifier is not $team"
		fi
		echo "signed $(basename "$f")"
	done
done

# ---- 2. the archives -----------------------------------------------------------
if [[ $app == 1 ]]; then
	for a in $archs; do
		tgz=$hub/longarm-darwin-$a.tar.gz
		x=$sec/tar-$a
		mkdir "$x"
		tar -xzf "$tgz" -C "$x"
		list=()
		while IFS= read -r e; do list+=("$e"); done < <(tar -tzf "$tgz")
		for t in longarm longarmd; do
			[[ -f $x/$t ]] || die "$tgz has no $t"
			cmp -s "$x/$t" "$sec/unsigned/$t-darwin-$a" || die "$tgz: its $t is not the release's $t-darwin-$a"
			cp "$hub/$t-darwin-$a" "$x/$t"
			chmod 0755 "$x/$t"
		done
		COPYFILE_DISABLE=1 tar --no-mac-metadata --uid 0 --gid 0 --uname root --gname wheel \
			-czf "$sec/new.tar.gz" -C "$x" "${list[@]}"
		mv "$sec/new.tar.gz" "$tgz"
		gname=longarm_${v}_darwin_${a}.tar.gz
		grep -q "  $gname\$" "$sums" || die "checksums/unix.txt has no $gname"
		awk -v s="$(sha "$tgz")" -v n="$gname" '$2 == n { $0 = s "  " n } { print }' "$sums" >"$sec/sums"
		cat "$sec/sums" >"$sums"
		echo "repacked $(basename "$tgz") with the signed binaries"
	done
fi

# ---- 3. the installers ---------------------------------------------------------
pkgs=()
for a in $archs; do
	b=$sec/pkg-$a
	mkdir "$b"
	cp "$hub/longarm-darwin-$a" "$b/longarm"
	cp "$hub/longarmd-darwin-$a" "$b/longarmd"
	p=$hub/longarm-darwin-$a.pkg
	if [[ $inst == 1 ]]; then
		VERSION=$v ARCH=$a BINDIR=$b OUT=$p PKG_IDENTITY=$inst_name KEYCHAIN=$inst_kc sh "$repo/packaging/macos/build-pkg.sh"
		pkgutil --check-signature "$p" | tee "$sec/pkgsig"
		if [[ -n $team ]]; then
			grep -q "Developer ID Installer: .*($team)" "$sec/pkgsig" || die "$p: not signed by team $team"
		fi
	else
		VERSION=$v ARCH=$a BINDIR=$b OUT=$p sh "$repo/packaging/macos/build-pkg.sh"
	fi
	pkgs+=("$p")
done

# ---- 4. notarize, staple ---------------------------------------------------------
notarized=no
if [[ $notary == 1 && $app == 1 ]]; then
	printf '%s\n' "$APPLE_NOTARY_API_KEY_P8" >"$sec/AuthKey.p8"
	auth=(--key "$sec/AuthKey.p8" --key-id "$APPLE_NOTARY_KEY_ID" --issuer "$APPLE_NOTARY_ISSUER_ID")
	mkdir "$sec/notarize" "$sec/binaries"
	cp "${signed_bins[@]}" "$sec/binaries/"
	ditto -c -k --keepParent "$sec/binaries" "$sec/notarize/binaries.zip"
	subs=("$sec/notarize/binaries.zip")
	if [[ $inst == 1 ]]; then subs+=("${pkgs[@]}"); fi
	# submit() <file> <log name>: waits for the verdict (in parallel below)
	submit() {
		local out=$sec/notarize/$2.json id status
		if ! xcrun notarytool submit "$1" "${auth[@]}" --wait --timeout 45m --output-format json >"$out" 2>"$sec/notarize/$2.err"; then
			echo "::error::notarytool submit $(basename "$1") failed: $(tr '\n' ' ' <"$sec/notarize/$2.err")"
		fi
		id=$(plutil -extract id raw -o - "$out" 2>/dev/null || true)
		status=$(plutil -extract status raw -o - "$out" 2>/dev/null || true)
		echo "notarization of $(basename "$1"): ${status:-no result} (submission ${id:-none})"
		if [[ $status != Accepted ]]; then
			[[ -z $id ]] || xcrun notarytool log "$id" "${auth[@]}" || true
			return 1
		fi
	}
	pids=() i=0
	for f in "${subs[@]}"; do
		submit "$f" "sub$i" &
		pids+=($!)
		i=$((i + 1))
	done
	fail=0
	for pid in "${pids[@]}"; do wait "$pid" || fail=1; done
	[[ $fail == 0 ]] || die "notarization failed (the log is above)"
	if [[ $inst == 1 ]]; then
		for p in "${pkgs[@]}"; do
			xcrun stapler staple "$p"
			xcrun stapler validate "$p"
			spctl --assess --type install -vv "$p" 2>&1 | tee "$sec/spctl"
			grep -q 'source=Notarized Developer ID' "$sec/spctl" || die "$p: Gatekeeper doesn't see it as notarized"
		done
		notarized="binaries and .pkg"
	else
		notarized="binaries"
	fi
fi

# ---- summary -------------------------------------------------------------------
state="binaries $([[ $app == 1 ]] && echo signed || echo UNSIGNED), pkg $([[ $inst == 1 ]] && echo signed || echo UNSIGNED), notarized: $notarized"
echo "$state" >"$done_marker"
{
	echo "### macOS artifacts"
	echo
	echo "$state"
	echo
	echo '| file | sha256 |'
	echo '|---|---|'
	for f in "${signed_bins[@]}" "$hub"/longarm-darwin-*.tar.gz "${pkgs[@]}"; do
		echo "| $(basename "$f") | \`$(sha "$f")\` |"
	done
} >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
echo "macos-sign: $state"
