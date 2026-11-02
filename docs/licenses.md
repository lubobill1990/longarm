# Licenses

Longarm is licensed under the [MIT License](../LICENSE): use, copy, modify
and redistribute it, commercially too, keeping the copyright notice.

## What the releases include

Everything Longarm links or bundles is under a permissive license (MIT,
BSD-2/3-Clause, Apache-2.0, ISC, Unlicense; the web UI's fonts under the SIL
Open Font License 1.1). No GPL, LGPL, AGPL, MPL or other copyleft code is
compiled into any binary, so nothing constrains Longarm's own license.

Those licenses still ask for their notices to travel with the binaries.
[`internal/notices/THIRD_PARTY_NOTICES.txt`](../internal/notices/THIRD_PARTY_NOTICES.txt)
holds Longarm's license and the full license texts (LICENSE, NOTICE,
PATENTS ...) of:

- every Go module linked into the released binaries (`cmd/...` on Windows,
  Linux and macOS, with the release build tags) and Go's standard library;
- the web UI's runtime packages (the hub panel and the tray), fonts included;
- the hub container image's Litestream (the version `deploy/hub/image-build.sh`
  installs) and the Go modules it links.

Where it goes:

| Where | How |
|---|---|
| every binary | `longarm licenses`, `longarmd licenses`, `longarm-hub licenses`, `longarm-relay licenses` (embedded) |
| hub panel | `/licenses.txt` (the front page's footer: Licenses) |
| tray | Settings → Licenses |
| archives (`.zip`, `.tar.gz`) | `LICENSE`, `THIRD_PARTY_NOTICES.txt` next to the binaries |
| MSI | `LICENSE.txt`, `THIRD_PARTY_NOTICES.txt` in the install folder |
| macOS `.pkg` | `/usr/local/share/longarm/` |
| deb / rpm | `/usr/share/doc/<package>/` |
| hub container image | `/usr/share/doc/longarm-hub/` |

The hub image is based on Google's distroless `static-debian12`, whose Debian
packages (ca-certificates, base-files, netbase, tzdata ...) are aggregated
with Longarm, not linked with it; their copyright files are in the image's
`/usr/share/doc`, their sources at https://sources.debian.org. Build-time
tools (Vite, TypeScript, WiX ...) are not distributed and don't apply.

## Changing a dependency

The file is generated, never edited by hand. After adding, removing or
upgrading a Go module, a web package or Litestream:

```sh
cd web && pnpm install && cd ..
go run ./packaging/notices      # rewrites internal/notices/THIRD_PARTY_NOTICES.txt
git add internal/notices/THIRD_PARTY_NOTICES.txt
```

CI's web job runs `go run ./packaging/notices -check`: it fails when the file
is out of date, and when a dependency's license is not one of the permissive
ones it knows. A copyleft or unrecognized license needs a decision before it
ships - usually: don't take the dependency. If it is fine (e.g. a dual
license, or a license file the classifier misreads), record the dependency
and the reason in `reviewed` in [`packaging/notices`](../packaging/notices/main.go).
