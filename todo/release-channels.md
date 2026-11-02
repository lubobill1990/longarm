# Release channels and updates (docs/ci-cd.md "Release channels")

## Deploying a hub on Azure Container Instances without a person

The hub follows preview, but the tenant refuses GitHub's OIDC tokens, so
release.yml can't deploy an ACI hub: the `hub-manual` job only prints the
`deploy.ps1 -Image ...:<version>` command. Like `deploy/azure/ci/ci-job.bicep`
(the scheduled e2e job), a Container Apps job with a managed identity in the
hub's resource group could do it: poll `GET /releases/latest.json?channel=preview`,
compare with `/healthz`'s version, run `deploy/azure/deploy.sh --image
ghcr.io/<owner>/longarm-hub:<version>` from that tag. It needs Owner (the
template assigns roles) on that resource group - the owner's call.

## Setting a worker's channel from the client

`longarm worker update <w> --auto on` is relayed to the worker's update
service (MachineUpdate.SetAuto); the channel isn't: today it is
`longarm exec <w> -- longarm-update channel dev`. Add `--channel` next to
`--auto` (a MachineUpdate RPC field and OpSetChannel in
internal/longarmd/machineupdate.go), owners only like `--auto`.

## The CLI's update notice ignores the channel

`Whoami.latest_version` (internal/hub/fleet.go, `latestNotice()`) is always
the newest stable, so `longarm` on a dev machine never says a newer dev
build exists (the tray and `longarm ls` do follow the channel). Send the
client's channel in `WhoamiRequest` (the update service's `MachineChannel()`
on Windows) and answer `latestNoticeIn(channel)`.

## Getting other users' machines updated

Admins can't update other users' MSI machines (by design: only owners,
through their update service), and 0.1.15 / 0.1.20 workers stay behind
until their owners click "Update" or turn automatic updates on. Options
worth a decision: show "N workers are k releases behind" to admins in the
panel, or turn automatic updates on by default for new MSI installs
(`AUTO_UPDATE=1`, off today).
