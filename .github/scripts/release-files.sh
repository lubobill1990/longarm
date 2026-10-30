#!/usr/bin/env bash
# release-files.sh <dir> <version>
# release.yml (publish, before signing; gather in a dry run): the release
# directory merged from the two build jobs (hub/ = packaging/hubrelease's flat
# directory + the MSIs, extra/ = the longarm-hub debs, checksums/<side>.txt =
# each side's GoReleaser checksums.txt). Merges the checksums into
# extra/checksums.txt (GoReleaser's format: sorted by file name, byte order);
# then the directory must hold exactly the files of the
# releases so far (the list below is v0.1.6's plus the macOS .pkg installers
# of build-macos, before signing: publish adds a <file>.minisig per hub/ file
# and SHA256SUMS), and checksums.txt must list
# exactly GoReleaser's 18 archives/packages, with the sums of the copies that
# are published under the hub's names.
set -euo pipefail
dir=${1:?usage: release-files.sh <dir> <version>} v=${2:?usage: release-files.sh <dir> <version>}
if [[ -d $dir/checksums ]]; then
	LC_ALL=C sort -k2,2 "$dir"/checksums/*.txt >"$dir/extra/checksums.txt"
	rm -r "$dir/checksums"
fi
fail=0
err() {
	echo "::error::$*"
	fail=1
}

expect_hub=$(
	sed "s/@V@/$v/g" <<'LIST'
longarm-darwin-amd64
longarm-darwin-amd64.pkg
longarm-darwin-amd64.tar.gz
longarm-darwin-arm64
longarm-darwin-arm64.pkg
longarm-darwin-arm64.tar.gz
longarm-hub-darwin-amd64
longarm-hub-darwin-arm64
longarm-hub-linux-amd64
longarm-hub-linux-arm64
longarm-hub-windows-amd64.exe
longarm-hub-windows-arm64.exe
longarm-linux-amd64
longarm-linux-amd64.deb
longarm-linux-amd64.rpm
longarm-linux-amd64.tar.gz
longarm-linux-arm64
longarm-linux-arm64.deb
longarm-linux-arm64.rpm
longarm-linux-arm64.tar.gz
longarm-relay-darwin-amd64
longarm-relay-darwin-arm64
longarm-relay-linux-amd64
longarm-relay-linux-arm64
longarm-relay-windows-amd64.exe
longarm-relay-windows-arm64.exe
longarm-tray-windows-amd64.exe
longarm-tray-windows-arm64.exe
longarm-windows-amd64.exe
longarm-windows-amd64.msi
longarm-windows-amd64.zip
longarm-windows-arm64.exe
longarm-windows-arm64.msi
longarm-windows-arm64.zip
longarmd-darwin-amd64
longarmd-darwin-arm64
longarmd-linux-amd64
longarmd-linux-arm64
longarmd-windows-amd64.exe
longarmd-windows-arm64.exe
LIST
)
expect_extra=$(printf '%s\n' checksums.txt "longarm-hub_${v}_amd64.deb" "longarm-hub_${v}_arm64.deb")
expect_sums=$(
	sed "s/@V@/$v/g" <<'LIST'
longarm-hub_@V@_amd64.deb
longarm-hub_@V@_arm64.deb
longarm-hub_@V@_darwin_amd64.tar.gz
longarm-hub_@V@_darwin_arm64.tar.gz
longarm-hub_@V@_linux_amd64.tar.gz
longarm-hub_@V@_linux_arm64.tar.gz
longarm-hub_@V@_windows_amd64.zip
longarm-hub_@V@_windows_arm64.zip
longarm_@V@_amd64.deb
longarm_@V@_amd64.rpm
longarm_@V@_arm64.deb
longarm_@V@_arm64.rpm
longarm_@V@_darwin_amd64.tar.gz
longarm_@V@_darwin_arm64.tar.gz
longarm_@V@_linux_amd64.tar.gz
longarm_@V@_linux_arm64.tar.gz
longarm_@V@_windows_amd64.zip
longarm_@V@_windows_arm64.zip
LIST
)

names() { (cd "$1" && find . -type f | sed 's|^\./||' | LC_ALL=C sort); }
same() { # <what> <expected> <actual>
	local d
	if ! d=$(diff <(LC_ALL=C sort <<<"$2") <(LC_ALL=C sort <<<"$3")); then
		err "$1 differs from the expected file names (< missing, > unexpected):"
		echo "$d"
	fi
}
[[ -z $(find "$dir" -mindepth 1 -maxdepth 1 ! -name hub ! -name extra) ]] || err "$dir has more than hub/ and extra/"
same "hub/" "$expect_hub" "$(names "$dir/hub")"
same "extra/" "$expect_extra" "$(names "$dir/extra")"
sums=$dir/extra/checksums.txt
same "checksums.txt" "$expect_sums" "$(awk '{print $2}' "$sums")"
[[ $(awk '{print $2}' "$sums") == "$(awk '{print $2}' "$sums" | LC_ALL=C sort)" ]] || err "checksums.txt is not sorted by name"

# checksums.txt against the files (GoReleaser's names -> the published copies)
checked=0
while read -r sum name; do
	case $name in
	longarm-hub_*.deb) f=extra/$name ;;
	longarm-hub_*) continue ;; # the hub archives are not published
	longarm_"$v"_*_*.*)
		rest=${name#"longarm_${v}_"} # linux_amd64.tar.gz
		f=hub/longarm-${rest%%_*}-${rest#*_}
		;;
	longarm_"$v"_*.*) f=hub/longarm-linux-${name#"longarm_${v}_"} ;; # amd64.deb
	*)
		err "checksums.txt: unexpected $name"
		continue
		;;
	esac
	if [[ ! -f $dir/$f ]]; then
		err "checksums.txt: $name -> $f is missing"
	elif [[ $(sha256sum "$dir/$f" | cut -d' ' -f1) != "$sum" ]]; then
		err "checksums.txt: $name differs from $f"
	else
		checked=$((checked + 1))
	fi
done <"$sums"

echo "$(names "$dir" | wc -l) files for $v; checksums.txt: $(wc -l <"$sums") entries, $checked checked against the published copies"
names "$dir"
exit "$fail"
