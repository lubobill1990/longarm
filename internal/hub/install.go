package hub

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/template"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/update"
)

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (h *Hub) releaseFile(w http.ResponseWriter, r *http.Request) {
	p, err := h.releasePath(r.Context(), r.PathValue("version"), r.PathValue("file"))
	if errors.Is(err, errReleaseMissing) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		if !errors.Is(err, errFetchBackoff) && r.Context().Err() == nil { // logged when the download failed
			h.log.Warn("release download", "path", r.URL.Path, "err", err)
		}
		w.Header().Set("Retry-After", "60")
		httpError(w, http.StatusServiceUnavailable, "release store unavailable, try again later")
		return
	}
	// shared open: a publish may replace the file while it is served
	f, err := platform.OpenShared(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// releasePath is a local path with version's file name (a release file of
// the index or its .minisig), fetched from the blob container if needed.
// errReleaseMissing: no such file, or no copy of it.
func (h *Hub) releasePath(ctx context.Context, version, name string) (string, error) {
	if !versionRE.MatchString(version) || !servableFile(name) {
		return "", errReleaseMissing
	}
	base := strings.TrimSuffix(name, update.SigExt)
	files, err := h.releaseFiles(version)
	if err != nil {
		return "", err
	}
	for _, r := range files {
		if r.File == base {
			return h.rel.file(ctx, r, base != name)
		}
	}
	return "", errReleaseMissing
}

// latestRelease returns the version install scripts hand out and its files
// by name: latest_release, unless the hub lost some of its files (then the
// newest stable version it has all files of; "" = none).
func (h *Hub) latestRelease() (string, map[string]store.Release, error) {
	latest, err := h.store.Meta("latest_release")
	if err != nil || latest == "" {
		return "", nil, err
	}
	rels, err := h.store.Releases()
	if err != nil {
		return "", nil, err
	}
	byVersion := map[string][]store.Release{}
	for _, r := range rels {
		byVersion[r.Version] = append(byVersion[r.Version], r)
	}
	complete := func(v string) bool {
		return len(byVersion[v]) > 0 && len(h.rel.missingFiles(byVersion[v])) == 0
	}
	v := latest
	if !complete(v) {
		v = ""
		var cands []string
		for c, files := range byVersion {
			if files[0].Channel == "stable" && complete(c) {
				cands = append(cands, c)
			}
		}
		sort.Slice(cands, func(i, j int) bool { return update.Compare(cands[i], cands[j]) > 0 })
		if len(cands) > 0 {
			v = cands[0]
		}
		h.log.Warn("install scripts: the latest release has missing files; publish it again", "latest", latest,
			"missing", h.rel.missingFiles(byVersion[latest]), "instead", cmp.Or(v, "(no release)"))
	}
	files := map[string]store.Release{}
	for _, r := range byVersion[v] {
		files[r.File] = r
	}
	return v, files, nil
}

// baseURL is the hub's external URL: public_url, else derived from the
// request (behind a proxy: X-Forwarded-Proto/Host).
func (h *Hub) baseURL(r *http.Request) string {
	if h.cfg.PublicURL != "" {
		return h.cfg.PublicURL
	}
	scheme, host := "http", h.forwardedHost(r)
	if p := r.Header.Get("X-Forwarded-Proto"); h.fromProxy(r) && (p == "https" || p == "http") {
		scheme = p
	}
	return scheme + "://" + host
}

// hubAddr is the gRPC address written into install scripts.
func (h *Hub) hubAddr(r *http.Request) string {
	if h.cfg.PublicAddr != "" {
		return h.cfg.PublicAddr
	}
	host := h.forwardedHost(r)
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	_, port, _ := net.SplitHostPort(h.Addr("grpc"))
	return net.JoinHostPort(host, port)
}

type scriptData struct {
	BaseURL, HubAddr, Fingerprint, Version string
	Hashes                                 []fileHash
}

type fileHash struct{ File, SHA256 string }

func (h *Hub) installScript(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, files, err := h.latestRelease()
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		d := scriptData{BaseURL: h.baseURL(r), HubAddr: h.hubAddr(r), Fingerprint: pki.Fingerprint(h.ca.Cert), Version: v}
		for _, f := range files {
			d.Hashes = append(d.Hashes, fileHash{f.File, f.SHA256})
		}
		sort.Slice(d.Hashes, func(i, j int) bool { return d.Hashes[i].File < d.Hashes[j].File })
		var buf bytes.Buffer
		t := shTmpl
		switch kind {
		case "ps1":
			t = ps1Tmpl
		case "uninstall-ps1":
			t = uninstallPs1Tmpl
		}
		if err := t.Execute(&buf, d); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(buf.Bytes())
	}
}

var shTmpl = template.Must(template.New("sh").Parse(`#!/bin/sh
# Longarm installer (Linux / macOS), generated by longarm-hub.
#   curl -fsSL {{.BaseURL}}/install.sh | sudo sh -s -- --join CODE [--role worker|client|both] [--name NAME]   (default role: worker)
#        [--prefix DIR] [--no-service] [--data-dir DIR] [--no-package]
#   curl -fsSL {{.BaseURL}}/install.sh | sudo sh -s -- --login [--role ...] [--label KEY=VALUE]...   (or LONGARM_LOGIN=1; default role: both)
# As root on Linux with a .deb/.rpm in the release: installs that package (its postinst joins
# and enables longarmd@<sudo user>). Otherwise downloads longarm and longarmd {{.Version}}, verifies their sha256, installs them,
# joins the fleet (pinning the hub CA {{.Fingerprint}})
# and, for workers, installs and starts the longarmd service.
# --login instead of a join code: sign in with your Microsoft account - longarmd login (workers,
# yours) / longarm login (clients) print a code to approve at {{.BaseURL}}/link in any browser;
# --role both (the default with --login): one code and one approval make this machine your worker and your client.
# --label KEY=VALUE (repeatable; or LONGARM_LABELS='k=v,k2=v2'), with --login: labels the hub records
# for the worker (longarmd login --label); a join code carries its own (longarm worker add --label).
set -eu
HUB_URL='{{.BaseURL}}'
HUB_ADDR='{{.HubAddr}}'
CA_FP='{{.Fingerprint}}'
VERSION='{{.Version}}'
ROLE=''; JOIN=''; NAME=''; PREFIX=''; SERVICE=1; DATA_DIR=''; USE_PKG=1; PREFIX_SET=0; LOGIN="${LONGARM_LOGIN:-0}"
LABELARGS=''; [ -z "${LONGARM_LABELS:-}" ] || LABELARGS="--label $LONGARM_LABELS"
while [ $# -gt 0 ]; do
  case "$1" in
    --join) JOIN="$2"; shift 2;;
    --login) LOGIN=1; shift;;
    --role) ROLE="$2"; shift 2;;
    --roles) ROLE="$2"; shift 2;;
    --name) NAME="$2"; shift 2;;
    --label) case "$2" in *[!A-Za-z0-9._/@:+,=-]*) echo "install.sh: bad label $2" >&2; exit 2;; esac
      LABELARGS="$LABELARGS --label $2"; shift 2;;
    --prefix) PREFIX="$2"; PREFIX_SET=1; shift 2;;
    --no-package) USE_PKG=0; shift;;
    --data-dir) DATA_DIR="$2"; shift 2;;
    --no-service) SERVICE=0; shift;;
    join) shift;;
    *) echo "install.sh: unknown argument $1" >&2; exit 2;;
  esac
done
[ "$LOGIN" = 1 ] || LOGIN=0
# default role: with --login this machine becomes your worker and your client (one sign-in); a join code: a worker
if [ -z "$ROLE" ]; then if [ "$LOGIN" = 1 ]; then ROLE=both; else ROLE=worker; fi; fi
[ "$LOGIN" = 0 ] || [ -z "$JOIN" ] || { echo "install.sh: --login and --join exclude each other" >&2; exit 2; }
[ -z "$LABELARGS" ] || [ "$LOGIN" = 1 ] || { echo "install.sh: --label goes with --login (a join code carries its labels: longarm worker add --label)" >&2; exit 2; }
case "${LONGARM_LABELS:-}" in *[!A-Za-z0-9._/@:+,=-]*) echo "install.sh: bad LONGARM_LABELS" >&2; exit 2;; esac
[ -n "$VERSION" ] || { echo "install.sh: the hub has no release yet (longarm-hub release add)" >&2; exit 1; }
case "$(uname -s)" in Linux) OS=linux;; Darwin) OS=darwin;; *) echo "unsupported OS $(uname -s)" >&2; exit 1;; esac
case "$(uname -m)" in x86_64|amd64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; *) echo "unsupported arch $(uname -m)" >&2; exit 1;; esac
if [ -z "$PREFIX" ]; then
  if [ "$(id -u)" = 0 ]; then PREFIX=/usr/local/bin; else PREFIX="$HOME/.local/bin"; fi
fi
sha_of() {
  case "$1" in
{{- range .Hashes}}
    {{.File}}) echo {{.SHA256}};;
{{- end}}
    *) echo "";;
  esac
}
sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
fetch() { # fetch FILE: download into $TMP and verify its sha256
  want=$(sha_of "$1")
  [ -n "$want" ] || { echo "install.sh: release $VERSION has no $1" >&2; exit 1; }
  echo "downloading $1 ($VERSION)"
  curl -fsSL "$HUB_URL/releases/$VERSION/$1" -o "$TMP/$1"
  got=$(sha256 "$TMP/$1")
  [ "$got" = "$want" ] || { echo "install.sh: sha256 mismatch for $1: $got != $want" >&2; exit 1; }
}
# As root on Linux with a package in the release: install it (systemd unit
# longarmd@<user>, upgrades through the package manager); its postinst joins.
PKG=''
if [ "$OS" = linux ] && [ "$(id -u)" = 0 ] && [ "$USE_PKG" = 1 ] && [ "$PREFIX_SET" = 0 ]; then
  if command -v dpkg >/dev/null 2>&1 && [ -n "$(sha_of longarm-linux-$ARCH.deb)" ]; then PKG=deb
  elif command -v rpm >/dev/null 2>&1 && [ -n "$(sha_of longarm-linux-$ARCH.rpm)" ]; then PKG=rpm; fi
fi
if [ -n "$PKG" ]; then
  f="longarm-linux-$ARCH.$PKG"; fetch "$f"
  U="${SUDO_USER:-}"
  [ -n "$U" ] || { [ -z "$JOIN" ] && [ "$LOGIN" = 0 ]; } || [ "$ROLE" = client ] || { echo "install.sh: run through sudo from the account longarmd should run as (or pass --no-package)" >&2; exit 1; }
  export LONGARM_ROLES="$ROLE" LONGARM_HUB="$HUB_ADDR" LONGARM_JOIN_CODE="$JOIN" LONGARM_CA_FINGERPRINT="$CA_FP" LONGARM_USER="$U"
  if [ "$PKG" = deb ]; then
    if command -v apt-get >/dev/null 2>&1; then apt-get install -y "$TMP/$f"; else dpkg -i "$TMP/$f"; fi
  else
    if command -v dnf >/dev/null 2>&1; then dnf install -y "$TMP/$f"; else rpm -Uvh "$TMP/$f"; fi
  fi
  echo "installed longarm $VERSION ($PKG)"
  if [ "$LOGIN" = 1 ]; then
    # The package's postinst had no code: sign in as the sudo user, then start its service.
    AS=''; [ -n "$U" ] && AS="sudo -H -u $U"
    NAMEARG=''; [ -n "$NAME" ] && NAMEARG="--name $NAME"
    # both: one code, one approval for the worker and this user's longarm
    if [ "$ROLE" != client ]; then
      $AS longarmd login "$HUB_ADDR" --ca-fingerprint "$CA_FP" --role "$ROLE" $NAMEARG $LABELARGS
      systemctl enable --now "longarmd@$U"
      echo "longarmd@$U started"
    else
      $AS longarm login "$HUB_ADDR" --ca-fingerprint "$CA_FP" $NAMEARG
    fi
  fi
  exit 0
fi
mkdir -p "$PREFIX"
for tool in longarm longarmd; do
  f="$tool-$OS-$ARCH"; fetch "$f"
  chmod 0755 "$TMP/$f"; mv "$TMP/$f" "$PREFIX/$tool"
done
echo "installed longarm and longarmd into $PREFIX"
[ -n "$JOIN" ] || [ "$LOGIN" = 1 ] || { echo "no --join code given: join later with: longarmd join $HUB_ADDR <code> --ca-fingerprint $CA_FP (or longarmd login $HUB_ADDR)"; exit 0; }
NAMEARG=''; [ -n "$NAME" ] && NAMEARG="--name $NAME"
DD=''; [ -n "$DATA_DIR" ] && DD="--data-dir $DATA_DIR"
U="${SUDO_USER:-$(id -un)}"
# The service runs as $U with $U's data dir (~$U/.longarm): enroll as $U
# too (root's own default data dir is /var/lib/longarmd on Linux).
AS=''
if [ "$SERVICE" = 1 ] && [ "$(id -u)" = 0 ] && [ "$U" != root ]; then AS="sudo -H -u $U"; fi
start_service() {
  if [ "$SERVICE" = 1 ]; then
    "$PREFIX/longarmd" service install --user "$U" $DD && "$PREFIX/longarmd" service start
    echo "longarmd service started$1"
  fi
}
if [ "$LOGIN" = 1 ]; then
  # A client's config belongs to the person, not root.
  CAS=''; if [ "$(id -u)" = 0 ] && [ "$U" != root ]; then CAS="sudo -H -u $U"; fi
  case "$ROLE" in
    client) $CAS "$PREFIX/longarm" login "$HUB_ADDR" --ca-fingerprint "$CA_FP" $NAMEARG ;;
    worker|both)
      # both, longarmd and longarm as the same account: one code, one approval
      R="$ROLE"; [ "$AS" = "$CAS" ] || R=worker
      $AS "$PREFIX/longarmd" login "$HUB_ADDR" --ca-fingerprint "$CA_FP" --role "$R" $NAMEARG $DD $LABELARGS
      start_service ""
      [ "$ROLE" = "$R" ] || $CAS "$PREFIX/longarm" login "$HUB_ADDR" --ca-fingerprint "$CA_FP" ;;
    *) echo "install.sh: unknown role $ROLE" >&2; exit 2;;
  esac
  exit 0
fi
[ "$ROLE" = both ] && ROLE=worker # the binary install sets up the worker; longarm join with a client code
case "$ROLE" in
  client) "$PREFIX/longarm" join "$HUB_ADDR" "$JOIN" --ca-fingerprint "$CA_FP" $NAMEARG ;;
  worker)
    $AS "$PREFIX/longarmd" join "$HUB_ADDR" "$JOIN" --ca-fingerprint "$CA_FP" --no-wait $NAMEARG $DD
    start_service "; it finishes enrolling once an admin runs: longarm admin approve <name>" ;;
  *) echo "install.sh: unknown role $ROLE" >&2; exit 2;;
esac
`))

// psElevate ("elevate"): Invoke-Elevated runs this hub's script again in an
// elevated PowerShell (UAC) with the given settings - in a file only this
// account reads, so a join code stays off command lines - and waits for
// that window (the sign-in code, the result; it waits for Enter). The
// elevated run refuses to go on as another account (a standard user who
// typed an administrator's password: -PerUser).
const psElevate = `{{define "elevate"}}function Invoke-Elevated([string]$Script, [hashtable]$Settings, [string]$Why, [string]$Declined) {
  $f = Join-Path ([IO.Path]::GetTempPath()) ('longarm-' + [guid]::NewGuid() + '.ps1')
  $lines = @('$env:LONGARM_ELEVATED_FOR = ''' + $meSid + '''')
  foreach ($k in $Settings.Keys) {
    $v = [string]$Settings[$k]
    if ($v) { $lines += '$env:' + $k + ' = ''' + $v.Replace('''', '''''') + '''' }
  }
  $lines += 'try { irm ''' + $HubUrl + '/' + $Script + ''' | iex } catch { Write-Host $_ -ForegroundColor Red } finally { Remove-Item -LiteralPath $PSCommandPath -ErrorAction SilentlyContinue; Read-Host ''Press Enter to close this window'' | Out-Null }'
  Set-Content -LiteralPath $f -Value $lines -Encoding UTF8
  Write-Host $Why
  try {
    Start-Process -FilePath (Get-Process -Id $PID).Path -Verb RunAs -Wait -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', ('"' + $f + '"'))
  } catch {
    Remove-Item -LiteralPath $f -ErrorAction SilentlyContinue
    throw $Declined
  }
}
if ($env:LONGARM_ELEVATED_FOR -and $env:LONGARM_ELEVATED_FOR -ne $meSid) {
  throw 'Windows elevated another account than the one that started this: run it in an administrator PowerShell of your own account, or for your account only with -PerUser ($env:LONGARM_PER_USER=1)'
}{{end}}`

var ps1Tmpl = template.Must(template.Must(template.New("ps1").Parse(psElevate)).Parse(`# Longarm installer (Windows), generated by longarm-hub.
#   $env:LONGARM_LOGIN=1; irm {{.BaseURL}}/install.ps1 | iex       sign in with Microsoft: no code, no password
#   $env:LONGARM_JOIN='K7P-Q2M'; irm {{.BaseURL}}/install.ps1 | iex
#   & ([scriptblock]::Create((irm {{.BaseURL}}/install.ps1))) -Login|-Join K7P-Q2M [-Role worker|client|both] [-Name N]
#        [-Run session|service] [-Listen URL[,URL]] [-PerUser] [-Prefix DIR] [-NoService] [-NoMsi] [-NoPath] [-NoFirewall]
#        [-NoTray] [-ServiceUser .\you] [-ServiceAccount LocalSystem] [-Label 'k=v,k2=v2']
#   Remove it again: irm {{.BaseURL}}/uninstall.ps1 | iex
# As an administrator, for this machine (the MSI): from a PowerShell that isn't elevated, Windows asks
# to elevate (UAC) and the install goes on in that window. -PerUser ($env:LONGARM_PER_USER=1): for this
# account only and without administrator rights (also -Prefix, -NoMsi, or a release without an MSI).
# Either way the Longarm tray starts afterwards and at every logon (-NoTray: not).
# Workers run as you (-Run session, the default; $env:LONGARM_RUN): longarmd starts at your logon (a
# scheduled task), restarts when it stops, and needs no Windows service and no password - it works when
# you sign in to Windows with Hello or a security key only. Jobs keep running while a remote desktop is
# disconnected and stop at logoff or reboot until you log on again. -Run service installs the longarmd
# service instead (runs with nobody logged on) as a user account (DESIGN §3.1; its password from
# $env:LONGARM_SERVICE_PASSWORD or a prompt), or -ServiceAccount LocalSystem ($env:LONGARM_SERVICE_ACCOUNT;
# jobs run as SYSTEM; not recommended). -NoService: no logon task and no service.
# Elevated and with an MSI in the release: installs the MSI {{.Version}} silently (sha256-verified;
# firewall rules, PATH, the tray; MODE=custom HUB=this hub RUN=session|service, LOGIN=1 or JOIN_CODE;
# extra MSI properties in $env:LONGARM_MSI_PROPS, e.g. ADMIN_WORKER=1); -Login then signs in here.
# Otherwise (or with -Prefix / -NoMsi): downloads longarm.exe and longarmd.exe (and longarm-tray.exe,
# the worker's windowless host, when the release has it), verifies their sha256, installs them, joins
# the fleet (pinning the hub CA {{.Fingerprint}}) and, for workers, registers the logon task (or the service).
# -Login ($env:LONGARM_LOGIN=1) instead of a join code: sign in with your Microsoft account -
# longarmd login (workers, yours) / longarm login (clients) print a code to approve at
# {{.BaseURL}}/link in any browser; -Role both (the default with -Login): one code and one approval for both.
# -Label 'k=v,k2=v2' ($env:LONGARM_LABELS): labels the hub records for the worker at its sign-in
# (longarmd login --label; the MSI records them as LABELS for the tray / longarmd login).
param(
  [string]$Join = $env:LONGARM_JOIN,
  [switch]$Login = ($env:LONGARM_LOGIN -eq '1'),
  [string]$Role = $env:LONGARM_ROLE,
  [string]$Name = $env:LONGARM_NAME,
  [switch]$PerUser = ($env:LONGARM_PER_USER -eq '1'),
  [string]$Prefix = $env:LONGARM_PREFIX,
  [string]$DataDir = $env:LONGARM_DATA_DIR,
  [string]$Listen = $env:LONGARM_LISTEN,
  [string]$Run = $env:LONGARM_RUN,
  [switch]$NoService = ($env:LONGARM_NO_SERVICE -eq '1'),
  [switch]$NoMsi = ($env:LONGARM_NO_MSI -eq '1'),
  [switch]$NoPath = ($env:LONGARM_NO_PATH -eq '1'),
  [switch]$NoFirewall = ($env:LONGARM_NO_FIREWALL -eq '1'),
  [switch]$NoTray = ($env:LONGARM_NO_TRAY -eq '1'),
  [string]$ServiceUser = $env:LONGARM_SERVICE_USER,
  [string]$ServiceAccount = $env:LONGARM_SERVICE_ACCOUNT,
  [string]$Label = $env:LONGARM_LABELS
)
# a service account (or LocalSystem) asks for a service
if (-not $Run) { $Run = if ($ServiceUser -or $ServiceAccount) { 'service' } else { 'session' } }
if ($Run -notin 'session', 'service') { throw "-Run ${Run}: want session or service" }
# default role: with -Login this machine becomes your worker and your client (one sign-in); a join code: a worker
if (-not $Role) { $Role = if ($Login) { 'both' } else { 'worker' } }
if (-not $ServiceUser) { $ServiceUser = ".\$env:USERNAME" }
function Get-ServicePassword {
  if ($env:LONGARM_SERVICE_PASSWORD) { return $env:LONGARM_SERVICE_PASSWORD }
  try {
    $sec = Read-Host -AsSecureString "Windows password of $ServiceUser (the longarmd service runs jobs as this account)"
  } catch {
    throw ('a worker''s longarmd service runs as ' + $ServiceUser + ' and needs its password: set $env:LONGARM_SERVICE_PASSWORD (or -ServiceAccount LocalSystem / $env:LONGARM_SERVICE_ACCOUNT=''LocalSystem'' to run it as SYSTEM, not recommended; or -Run session: no password)')
  }
  return [Runtime.InteropServices.Marshal]::PtrToStringAuto([Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec))
}
$asSystem = $ServiceAccount -eq 'LocalSystem'
if ($ServiceAccount -and -not $asSystem) { throw "-ServiceAccount takes only LocalSystem (use -ServiceUser for an account)" }
if ($Login -and $Join) { throw '-Login and -Join exclude each other' }
if ($Label -and $Join) { throw '-Label goes with a sign-in (-Login, or the MSI''s LOGIN=1); a join code carries its labels: longarm worker add --label' }
if ($Label -and $Label -notmatch '^[A-Za-z0-9._/@:+,=-]+$') { throw "bad -Label $Label" }
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$HubUrl = '{{.BaseURL}}'
$HubAddr = '{{.HubAddr}}'
$CaFp = '{{.Fingerprint}}'
$Version = '{{.Version}}'
$Hashes = @{
{{- range .Hashes}}
  '{{.File}}' = '{{.SHA256}}'
{{- end}}
}
if (-not $Version) { throw 'the hub has no release yet (longarm-hub release add)' }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
$meSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
{{template "elevate" .}}
$msi = "longarm-windows-$arch.msi"
$nameArgs = @(); if ($Name) { $nameArgs = @('--name', $Name) }
$ddArgs = @(); if ($DataDir) { $ddArgs = @('--data-dir', $DataDir) }
$labelArgs = @(); if ($Label) { $labelArgs = @('--label', $Label) }
if ($Role -notin 'worker', 'client', 'both') { throw "unknown role $Role" }
# for this machine, as an administrator (the MSI) - unless this account only was asked for
$perUserInstall = $PerUser -or $Prefix -or $NoMsi -or -not $Hashes[$msi]
if (-not $admin -and -not $perUserInstall) {
  Invoke-Elevated 'install.ps1' @{
    LONGARM_JOIN = $Join; LONGARM_LOGIN = $(if ($Login) { '1' }); LONGARM_ROLE = $Role; LONGARM_NAME = $Name
    LONGARM_DATA_DIR = $DataDir; LONGARM_LISTEN = $Listen; LONGARM_RUN = $Run; LONGARM_LABELS = $Label
    LONGARM_NO_SERVICE = $(if ($NoService) { '1' }); LONGARM_NO_PATH = $(if ($NoPath) { '1' })
    LONGARM_NO_FIREWALL = $(if ($NoFirewall) { '1' }); LONGARM_NO_TRAY = $(if ($NoTray) { '1' })
    LONGARM_SERVICE_USER = $ServiceUser; LONGARM_SERVICE_ACCOUNT = $ServiceAccount; LONGARM_MSI_PROPS = $env:LONGARM_MSI_PROPS
  } 'Longarm installs for this machine as an administrator: approve the Windows prompt; the install goes on in that window. (For this account only, without administrator rights: -PerUser, or $env:LONGARM_PER_USER=1.)' 'not elevated (the Windows prompt was declined): run it in an administrator PowerShell, or for this account only with -PerUser ($env:LONGARM_PER_USER=1)'
  return
}
$machineWide = $admin -and -not $PerUser
if ($admin -and $Hashes[$msi] -and -not $NoMsi -and -not $Prefix -and -not $PerUser) {
  # The MSI: longarm, longarmd, the tray, firewall rules, PATH, the worker (logon task or service), clean uninstall.
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("longarm-" + [guid]::NewGuid() + ".msi")
  Write-Host "downloading $msi ($Version)"
  Invoke-WebRequest -UseBasicParsing -Uri "$HubUrl/releases/$Version/$msi" -OutFile $tmp
  $got = (Get-FileHash -Algorithm SHA256 $tmp).Hash.ToLower()
  if ($got -ne $Hashes[$msi]) { Remove-Item $tmp; throw "sha256 mismatch for ${msi}: $got != $($Hashes[$msi])" }
  $props = @('MODE=custom', "ROLES=$Role", "HUB=$HubAddr", "CA_FINGERPRINT=$CaFp", "RUN=$Run")
  if ($Join) { $props += "JOIN_CODE=$Join" }
  if ($Login) { $props += 'LOGIN=1'; $props += 'LAUNCH_TRAY=0' } # signs in below, in this console
  if ($Name) { $props += "NODE_NAME=$Name" }
  if ($Listen) { $props += "LISTEN=$Listen" }
  if ($DataDir) { $props += "DATADIR=$DataDir" }
  if ($Label) { $props += "LABELS=$Label" }
  if ($Role -ne 'client' -and $Run -eq 'service') {
    if ($asSystem) {
      Write-Warning 'the longarmd service runs as LocalSystem: jobs run as SYSTEM, without your git / npm / claude settings'
      $props += 'SERVICE_ACCOUNT=LocalSystem'
    } else {
      # The service logs on as the user (DESIGN §3.1); the MSI grants "Log on as a service".
      $props += "SERVICE_USER=$ServiceUser"; $props += "SERVICE_PASSWORD=$(Get-ServicePassword)"
    }
  }
  if ($env:LONGARM_MSI_PROPS) { $props += ($env:LONGARM_MSI_PROPS -split '\s+' | Where-Object { $_ }) }
  $log = Join-Path ([IO.Path]::GetTempPath()) 'longarm-msi.log'
  # Each property as PROP="value" (quotes doubled): spaces or quotes in a value can't add properties.
  $quoted = $props | ForEach-Object { $k, $v = $_ -split '=', 2; $k + '="' + ($v -replace '"', '""') + '"' }
  $argLine = '/i "' + $tmp + '" /qn /norestart /l*v "' + $log + '" ' + ($quoted -join ' ')
  $p = Start-Process msiexec.exe -Wait -PassThru -ArgumentList $argLine
  Remove-Item $tmp -ErrorAction SilentlyContinue
  if ($p.ExitCode -notin 0, 3010) { throw "msiexec failed ($($p.ExitCode)); log: $log" }
  Write-Host "installed longarm $Version ($Role, worker: $Run) with the MSI; uninstall from Apps or: msiexec /x $msi"
  $bin = Join-Path $env:ProgramFiles 'Longarm'
  if ($Login) {
    if ($Role -ne 'client') {
      # the user-session worker's data dir is yours (~\.longarm); a service's is the MSI's
      $dd = if ($Run -eq 'session') { @('--data-dir', (Join-Path $env:USERPROFILE '.longarm')) } else { @() }
      & (Join-Path $bin 'longarmd.exe') login $HubAddr --ca-fingerprint $CaFp --role $Role @nameArgs @dd @labelArgs
      if ($LASTEXITCODE) { throw "longarmd login failed ($LASTEXITCODE)" }
    } else {
      & (Join-Path $bin 'longarm.exe') login $HubAddr --ca-fingerprint $CaFp @nameArgs
      if ($LASTEXITCODE) { throw "longarm login failed ($LASTEXITCODE)" }
    }
    $trayExe = Join-Path $bin 'longarm-tray.exe'
    if (Test-Path $trayExe) {
      # now, in your session and not elevated (this console is): Explorer starts it as you, as the
      # MSI's task does when it signs in itself
      Start-Process explorer.exe -ArgumentList ('"' + $trayExe + '"')
      Write-Host 'signed in; the Longarm tray is starting (on Windows 11 its icon may be under the ^ arrow next to the clock)'
    } else {
      Write-Host 'signed in'
    }
  }
  if ($Join -and $Role -ne 'client') { Write-Host 'the worker finishes enrolling once an admin runs: longarm admin approve <name>' }
  return
}
if (-not $Prefix) { $Prefix = if ($machineWide) { Join-Path $env:ProgramFiles 'Longarm' } else { Join-Path $env:LOCALAPPDATA 'Programs\Longarm' } }
New-Item -ItemType Directory -Force -Path $Prefix | Out-Null
$tools = @('longarm', 'longarmd')
if ($Hashes["longarm-tray-windows-$arch.exe"]) { $tools += 'longarm-tray' } # the tray, and the logon task's windowless host
foreach ($tool in $tools) {
  $f = "$tool-windows-$arch.exe"
  $want = $Hashes[$f]
  if (-not $want) { throw "release $Version has no $f" }
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("longarm-" + [guid]::NewGuid() + ".exe")
  Write-Host "downloading $f ($Version)"
  Invoke-WebRequest -UseBasicParsing -Uri "$HubUrl/releases/$Version/$f" -OutFile $tmp
  $got = (Get-FileHash -Algorithm SHA256 $tmp).Hash.ToLower()
  if ($got -ne $want) { Remove-Item $tmp; throw "sha256 mismatch for ${f}: $got != $want" }
  Move-Item -Force $tmp (Join-Path $Prefix "$tool.exe")
}
if (-not $NoPath) {
  $scope = if ($machineWide) { 'Machine' } else { 'User' }
  $path = [Environment]::GetEnvironmentVariable('Path', $scope)
  if (($path -split ';') -notcontains $Prefix) { [Environment]::SetEnvironmentVariable('Path', "$path;$Prefix", $scope) }
}
$env:Path = "$env:Path;$Prefix"
Write-Host "installed $($tools -join ', ') into $Prefix"
# Windows Firewall drops unsolicited inbound UDP to programs without a rule:
# hole punching (longnet direct paths) needs one for longarm.exe (longarmd's comes with
# its service).
if ($NoFirewall) { }
elseif ($admin) { & (Join-Path $Prefix 'longarm.exe') firewall allow }
else { Write-Host 'note: direct (hole-punched) connections need a firewall rule for longarm.exe: run "longarm firewall allow" in an elevated PowerShell' }
# the tray, like the MSI's: at every logon (the Run key) and in the Start menu - for this account,
# or for everybody when installed for this machine
function Install-Tray {
  $tray = Join-Path $Prefix 'longarm-tray.exe'
  if ($NoTray -or -not (Test-Path -LiteralPath $tray)) { return }
  $hive = if ($machineWide) { 'HKLM' } else { 'HKCU' }
  New-ItemProperty -Path ($hive + ':\Software\Microsoft\Windows\CurrentVersion\Run') -Name 'longarm-tray' -Value ('"' + $tray + '"') -PropertyType String -Force | Out-Null
  $menu = [Environment]::GetFolderPath($(if ($machineWide) { 'CommonPrograms' } else { 'Programs' }))
  $lnk = (New-Object -ComObject WScript.Shell).CreateShortcut((Join-Path $menu 'Longarm.lnk'))
  $lnk.TargetPath = $tray; $lnk.WorkingDirectory = $Prefix; $lnk.Description = 'Longarm tray'; $lnk.Save()
}
# Start-Tray starts it now, as you and not elevated (from an elevated console through Explorer)
function Start-Tray {
  $tray = Join-Path $Prefix 'longarm-tray.exe'
  if ($NoTray -or -not (Test-Path -LiteralPath $tray)) { return }
  if ($admin) { Start-Process explorer.exe -ArgumentList ('"' + $tray + '"') } else { Start-Process -FilePath $tray -ArgumentList '--signin' }
  Write-Host 'the Longarm tray is starting (on Windows 11 its icon may be under the ^ arrow next to the clock)'
}
Install-Tray
if (-not $Join -and -not $Login) {
  Start-Tray
  Write-Host "no join code: sign in from the Longarm tray, or later with: longarmd login $HubAddr (a join code: longarmd join $HubAddr <code> --ca-fingerprint $CaFp)"
  return
}
function Install-Worker([string]$note) {
  if ($NoService) { return }
  if ($Run -eq 'session') {
    # the logon task for you: no service, no password, no elevation needed
    & (Join-Path $Prefix 'longarmd.exe') session-mode install @ddArgs
    if ($LASTEXITCODE) { throw "longarmd session-mode install failed ($LASTEXITCODE)" }
    Write-Host "the worker runs as you from now on and at every logon$note"
    return
  }
  if (-not $admin) { throw 'installing the service needs an elevated PowerShell (or -Run session, the default)' }
  if ($asSystem) {
    & (Join-Path $Prefix 'longarmd.exe') service install @ddArgs   # warns: LocalSystem
  } else {
    $env:LONGARM_SERVICE_PASSWORD = Get-ServicePassword
    & (Join-Path $Prefix 'longarmd.exe') service install --user $ServiceUser @ddArgs
  }
  if ($LASTEXITCODE) { throw "longarmd service install failed ($LASTEXITCODE)" }
  & (Join-Path $Prefix 'longarmd.exe') service start
  Write-Host "longarmd service started$note"
}
if ($Login) {
  if ($Role -ne 'client') {
    # both: one code, one approval for the worker and your longarm
    & (Join-Path $Prefix 'longarmd.exe') login $HubAddr --ca-fingerprint $CaFp --role $Role @nameArgs @ddArgs @labelArgs
    if ($LASTEXITCODE) { throw "longarmd login failed ($LASTEXITCODE)" }
    Install-Worker ''
  } else {
    & (Join-Path $Prefix 'longarm.exe') login $HubAddr --ca-fingerprint $CaFp @nameArgs
    if ($LASTEXITCODE) { throw "longarm login failed ($LASTEXITCODE)" }
  }
  Start-Tray
  return
}
if ($Role -eq 'both') { $Role = 'worker' } # binary install: the worker; longarm join with a client code
switch ($Role) {
  'client' { & (Join-Path $Prefix 'longarm.exe') join $HubAddr $Join --ca-fingerprint $CaFp @nameArgs; if ($LASTEXITCODE) { throw "longarm join failed ($LASTEXITCODE)" } }
  'worker' {
    & (Join-Path $Prefix 'longarmd.exe') join $HubAddr $Join --ca-fingerprint $CaFp --no-wait @nameArgs @ddArgs
    if ($LASTEXITCODE) { throw "longarmd join failed ($LASTEXITCODE)" }
    Install-Worker '; it finishes enrolling once an admin runs: longarm admin approve <name>'
  }
  default { throw "unknown role $Role" }
}
Start-Tray
`))

var uninstallPs1Tmpl = template.Must(template.Must(template.New("uninstall-ps1").Parse(psElevate)).Parse(`# Longarm uninstaller (Windows), generated by longarm-hub: install.ps1's counterpart.
#   irm {{.BaseURL}}/uninstall.ps1 | iex
#   & ([scriptblock]::Create((irm {{.BaseURL}}/uninstall.ps1))) [-KeepData] [-Prefix DIR] [-DataDir DIR]
# Removes Longarm from this machine however install.ps1 (or the MSI) put it there:
# - the MSI "Longarm" (as an administrator: Windows asks to elevate): msiexec /x, which takes every
#   worker it installed off the hub (longarmd leave --kill-jobs) and removes their tasks and services,
#   the firewall rules, the Path entry, the tray's autostart and the files;
# - an install without the MSI (-Prefix; default %LOCALAPPDATA%\Programs\Longarm, and C:\Program Files\Longarm
#   when no MSI owns it): the tray (its autostart and Start menu entry), the logon task or the longarmd
#   service, the worker (longarmd leave --kill-jobs --purge), the firewall rule, the Path entry, the files;
# - and either way this machine's client sign-in (longarm leave): the hub revokes its certificates.
# -Prefix DIR: only the install in DIR - the MSI and other installs stay.
# -KeepData ($env:LONGARM_KEEP_DATA=1): keeps the worker's data dir (jobs, mirrors) and longarm's config.
param(
  [switch]$KeepData = ($env:LONGARM_KEEP_DATA -eq '1'),
  [string]$Prefix = $env:LONGARM_PREFIX,
  [string]$DataDir = $env:LONGARM_DATA_DIR
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$HubUrl = '{{.BaseURL}}'
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
$meSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
{{template "elevate" .}}
$ic = [StringComparison]::OrdinalIgnoreCase
# -Prefix names the one install to remove: the MSI stays then
$product = $null
if (-not $Prefix) {
  $product = Get-ItemProperty 'HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*' -ErrorAction SilentlyContinue |
    Where-Object { $_.DisplayName -eq 'Longarm' -and $_.PSChildName -like '{*}' } | Select-Object -First 1
}
$msiDir = ''
if ($product) {
  $msiDir = (Get-ItemProperty 'HKLM:\Software\Longarm' -ErrorAction SilentlyContinue).InstallDir
  if (-not $msiDir) { $msiDir = Join-Path $env:ProgramFiles 'Longarm' }
  $msiDir = $msiDir.TrimEnd('\')
}
$dirs = if ($Prefix) { @($Prefix) } else { @((Join-Path $env:LOCALAPPDATA 'Programs\Longarm'), (Join-Path $env:ProgramFiles 'Longarm')) }
$dirs = @($dirs | ForEach-Object { $_.TrimEnd('\') } | Where-Object { $_ -ine $msiDir } |
  Where-Object { (Test-Path -LiteralPath (Join-Path $_ 'longarmd.exe')) -or (Test-Path -LiteralPath (Join-Path $_ 'longarm.exe')) })
if (-not $product -and $dirs.Count -eq 0) { Write-Host 'Longarm is not installed on this machine'; return }
$forMachine = [bool]$product -or [bool]($dirs | Where-Object { $_.StartsWith($env:ProgramFiles, $ic) })
if ($forMachine -and -not $admin) {
  Invoke-Elevated 'uninstall.ps1' @{ LONGARM_KEEP_DATA = $(if ($KeepData) { '1' }); LONGARM_PREFIX = $Prefix; LONGARM_DATA_DIR = $DataDir } 'Longarm is installed for this machine: removing it needs an administrator - approve the Windows prompt; it goes on in that window.' 'not elevated (the Windows prompt was declined): run it in an administrator PowerShell'
  return
}
$ddArgs = @(); if ($DataDir) { $ddArgs = @('--data-dir', $DataDir) }
# Remove-Client: the hub revokes this machine's client certificate; the config goes too (not with -KeepData)
function Remove-Client([string]$dir) {
  $exe = Join-Path $dir 'longarm.exe'
  if (-not (Test-Path -LiteralPath $exe)) { return }
  & $exe leave
  $cfg = if ($env:LONGARM_CONFIG_DIR) { $env:LONGARM_CONFIG_DIR } else { Join-Path $HOME '.config\longarm' }
  if (-not $KeepData) { Remove-Item -LiteralPath $cfg -Recurse -Force -ErrorAction SilentlyContinue }
}
if ($product) {
  Remove-Client $msiDir
  $log = Join-Path ([IO.Path]::GetTempPath()) 'longarm-msi-uninstall.log'
  $msiArgs = '/x ' + $product.PSChildName + ' /qn /norestart /l*v "' + $log + '"'
  if ($KeepData) { $msiArgs += ' KEEP_DATA=1' }
  Write-Host ('removing the MSI ' + $product.DisplayVersion + ' (it takes its workers off the hub)')
  $p = Start-Process msiexec.exe -Wait -PassThru -ArgumentList $msiArgs
  if ($p.ExitCode -notin 0, 3010, 1605) { throw "msiexec /x failed ($($p.ExitCode)); log: $log" }
}
foreach ($dir in $dirs) {
  # the tray first: it hosts the user-session worker and would start it again
  Get-Process longarm-tray -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.Path.StartsWith($dir, $ic) } | Stop-Process -Force -ErrorAction SilentlyContinue
  foreach ($hive in 'HKCU', 'HKLM') {
    $run = $hive + ':\Software\Microsoft\Windows\CurrentVersion\Run'
    $v = (Get-ItemProperty -Path $run -Name 'longarm-tray' -ErrorAction SilentlyContinue).'longarm-tray'
    if ($v -and $v.IndexOf($dir, $ic) -ge 0 -and ($hive -eq 'HKCU' -or $admin)) { Remove-ItemProperty -Path $run -Name 'longarm-tray' }
  }
  foreach ($menu in [Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('CommonPrograms')) {
    $lnk = Join-Path $menu 'Longarm.lnk'
    if (Test-Path -LiteralPath $lnk) {
      $target = (New-Object -ComObject WScript.Shell).CreateShortcut($lnk).TargetPath
      if ($target -and $target.StartsWith($dir, $ic)) { Remove-Item -LiteralPath $lnk -Force -ErrorAction SilentlyContinue }
    }
  }
  $d = Join-Path $dir 'longarmd.exe'
  if (Test-Path -LiteralPath $d) {
    # the logon task (it stops the worker) - only this install's: the MSI's has the same name
    $task = Get-ScheduledTask -TaskName ('LongarmWorker-' + $meSid) -ErrorAction SilentlyContinue
    if ($task -and ($task.Actions | Where-Object { $_.Execute -and $_.Execute.Trim('"').StartsWith($dir, $ic) })) { & $d session-mode uninstall @ddArgs }
    $svc = Get-CimInstance Win32_Service -Filter "Name='longarmd'" -ErrorAction SilentlyContinue
    if ($admin -and $svc -and $svc.PathName -and $svc.PathName.IndexOf($dir, $ic) -ge 0) { & $d service uninstall }
    $leave = @('leave', '--kill-jobs', '--force'); if (-not $KeepData) { $leave += '--purge' }
    & $d @leave @ddArgs                   # the hub revokes the worker; --purge deletes its data dir
  }
  Remove-Client $dir
  if ($admin -and (Test-Path -LiteralPath (Join-Path $dir 'longarm.exe'))) { & (Join-Path $dir 'longarm.exe') firewall remove }
  foreach ($scope in 'User', 'Machine') {
    if ($scope -eq 'Machine' -and -not $admin) { continue }
    $path = [Environment]::GetEnvironmentVariable('Path', $scope)
    if (-not $path) { continue }
    $kept = ($path -split ';' | Where-Object { $_ -and $_.TrimEnd('\') -ine $dir }) -join ';'
    if ($kept -ne $path) { [Environment]::SetEnvironmentVariable('Path', $kept, $scope) }
  }
  Remove-Item -LiteralPath $dir -Recurse -Force
  Write-Host "removed $dir"
}
Write-Host 'Longarm is removed from this machine'
`))
