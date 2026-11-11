# Moving large data

Longarm's file commands are resumable and verified, but their speed is the path's: `longarm speedtest <a> [<b>]` reports it (a few seconds each way; the result guards `put`/`get`/`cp`/`sync` for 10 minutes). Over a direct path, Longarm is usually the simplest carrier at any size. Over a relayed path (`relay`, `derp`, `peer-relay`), the bytes go through a shared relay that is rate-limited per machine, so larger data should go another way.

Routes that keep Longarm out of the data path, in rough order of how often they fit:

- **Code and history**: a git remote the worker can reach. `--git-auth forward` (run/sync/handoff `--repo`) lets the worker fetch with your short-lived credential, so only the unpushed delta crosses Longarm.
- **Data that came from somewhere**: fetch it again on the worker from its source (release URL, package registry, dataset bucket) with `longarm exec`.
- **Cloud storage both ends already use**: an Azure blob container with a short-lived SAS URL and `azcopy`, or an S3/GCS bucket, works from any worker that has the CLI. A synced drive (OneDrive, Dropbox) works only if both machines sync the same account, and it may take minutes before the file appears on the other side.

Check that the route exists on both ends before relying on it (`longarm exec <w> -- az version`, the synced folder's path), and don't commit data to a git repository just to move it - it stays in the history.

Data that leaves the two machines this way is packed and encrypted first, for example `tar -c dir | zstd | age -p > dir.tar.zst.age`, or `7z a -p -mhe=on`. Send the passphrase or key through Longarm (`--secret-env`, or `longarm put` of a small file), never alongside the data, and delete the uploaded copy once the worker has it.
