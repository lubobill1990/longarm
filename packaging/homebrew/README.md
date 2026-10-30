# Homebrew tap (template)

GoReleaser renders a cask for `longarm` and `longarmd` (`homebrew_casks` in
`.goreleaser.yaml`, `skip_upload: true`): after `goreleaser release` it is
in `dist/homebrew/Casks/longarm.rb`. To publish:

1. Create the tap repo `your-org/homebrew-tap` (public or private).
2. Set `skip_upload: false` and a `HOMEBREW_TAP_GITHUB_TOKEN` (repo scope),
   or copy `dist/homebrew/Casks/longarm.rb` into the tap's `Casks/` by hand.
3. Users: `brew install your-org/tap/longarm && longarmd join <hub> <code>`
   (worker as a LaunchDaemon: `sudo longarmd service install --user $USER`).

The cask installs the binaries of `longarm-darwin-<arch>.tar.gz`, which a
release with the Apple credentials signs (Developer ID, hardened runtime) and
notarizes; for MDM-managed Macs use the signed `.pkg` (`packaging/macos`). Homebrew installs
update through `brew upgrade` (longarmd's self-update can't write into the
Caskroom; set `[update] disabled = true`).
