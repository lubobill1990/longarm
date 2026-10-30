---
name: longarm-release
description: How Longarm is released - the stable / preview / dev channels, what each push or tag publishes, how to cut a release candidate or a release, check a dev build reached the hub, promote, stop or roll back. Use when asked to release, publish, tag, ship to dev/preview/stable, check what version users get, or debug release.yml / deploy.yml.
---

# Releasing Longarm

Full reference: `docs/ci-cd.md` ("Release channels", "Cutting a release",
"Rolling back"). Channel logic: `internal/update/channel.go`. Workflow:
`.github/workflows/release.yml`.

## Channels (most tested first)

| channel | comes from | version | who gets it |
|---|---|---|---|
| stable | tag `vX.Y.Z` on main | `X.Y.Z` | everybody (default) |
| preview | tag `vX.Y.Z-rc.N` | `X.Y.Z-rc.N` | machines on preview or dev |
| dev | every push to main (not docs-only) | `<next patch>-dev.<run>` | machines on dev |

A machine takes the newest release of its channel or a more tested one.
Machines choose in the tray (Settings → Updates → Release channel; owner or
local admin) or with `[update] channel` in longarmd.toml. `beta` = `preview`.

## What each trigger does

- **Merge to main** → release.yml `meta` computes the dev version (next patch
  after the highest `v*` tag) → builds → signs → uploads to the hub on
  channel dev. No GitHub Release, no hub deploy, no fleet rollout. Nothing to
  do by hand.
- **`vX.Y.Z-rc.N` tag** → same, channel preview, plus a GitHub prerelease.
- **`vX.Y.Z` tag** (must be on main) → channel stable, GitHub Release, hub
  deploy (SSH forced command, auto rollback), canary rollout, image `:latest`.

## Cutting a release candidate / release

1. `git fetch --tags && git checkout main && git pull`; make sure the main
   CI run of HEAD is green and a dev build of it exists (below).
2. Pick the version: the next patch after the highest tag (the dev builds
   already carry it: `0.1.22-dev.N` → `v0.1.22-rc.1`, then `v0.1.22`). Never
   re-tag or reuse a version; never tag `-dev` (meta refuses it).
3. `git tag -a v0.1.22-rc.1 -m "Longarm 0.1.22-rc.1" && git push origin v0.1.22-rc.1`
4. Watch: `gh run watch $(gh run list -w release.yml -L1 --json databaseId -q '.[0].databaseId')`.
5. A stable tag only when the rc ran on preview machines without problems.

Tagging and pushing tags publishes to users: confirm with the user first.

## Checking what the hub serves

```sh
curl -s https://<hub>/releases/latest.json?channel=dev | jq .version     # also preview, stable (no param)
longarm admin release ls        # admin or release certificate: versions, channels, missing files
longarm admin release status    # the current / last rollout
longarm ls                      # workers; ↑ = a newer release of their channel exists
```

An older hub ignores `?channel=` and answers stable: channels only work once
the hub runs a version that has them (stable releases deploy the hub;
otherwise deploy.yml → hub deploy <version>).

## When something goes wrong

- Build/publish failed → **Re-run failed jobs** (never "Re-run all jobs" after
  publish succeeded: the MSIs would differ and publish refuses them).
- A bad dev build → merge the fix; the next dev build supersedes it (dev
  machines only move forward).
- A bad stable release → cancel / `longarm admin release abort`, then roll
  forward with the next patch tag. Downgrades are admin-only
  (`longarm admin release push <old> --allow-downgrade`).

## Invariants to keep when changing the pipeline

- Versions must sort dev < rc < release < next dev (`update.Compare`;
  `TestChannelVersionOrder`).
- MSIs of one X.Y.Z share the MSI version: keep `AllowSameVersionUpgrades`
  on `MajorUpgrade` (`e2e/msi_wxs_test.go`).
- Dev builds never deploy the hub, roll out, or create GitHub Releases;
  they run in concurrency group `longarm-release-dev`.
- The gRPC API only grows (old hubs/workers ignore new fields).
- Check workflow edits with actionlint and shellcheck (docs/ci-cd.md §5).
