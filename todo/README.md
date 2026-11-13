# todo

Known follow-ups that were deliberately left out of a change: one file per
area, one `##` section per item.

Each item says what is missing, why it matters, and where to start
(files, functions, the spec section). When you finish an item, delete its
section in the same pull request; delete the file when it is empty. When
you leave something undone on purpose, add it here rather than only in a
pull request description.

| file | area |
|---|---|
| [agent-sessions-ui.md](agent-sessions-ui.md) | the tray's Sessions window (docs/agent-sessions-ui.md) |
| [steerable-runs.md](steerable-runs.md) | `longarm run --steerable` (docs/steerable-runs.md) |
| [updates.md](updates.md) | a worker's update as the hub shows it (docs/usage.md "While a worker updates") |
| [remote-desktop.md](remote-desktop.md) | `longarm desktop` (docs/usage.md "Remote desktop") |
| [release-channels.md](release-channels.md) | release channels and updates (docs/ci-cd.md "Release channels") |
| [install-scripts.md](install-scripts.md) | the hub's install.ps1 / install.sh / uninstall.ps1 (docs/usage.md "Uninstall") |
| [ui-open.md](ui-open.md) | `longarm ui open` and the worker's fresh PATH (docs/usage.md "Opening Longarm's UI", "Operations") |
| [toolchains.md](toolchains.md) | older-worker owner-tool compatibility |
| [ci-flakes.md](ci-flakes.md) | tests that failed once and passed on a rerun |
| [agent-env.md](agent-env.md) | `--agent-env` of runs: the tray, the MCP tools, a default (docs/usage.md "Agent environment") |
| [copy.md](copy.md) | `longarm cp` between workers (docs/usage.md "Copy between workers") |
| [tray-signin.md](tray-signin.md) | the tray's sign-in state: refused admin workers, per-part expiry, reconnects |
| [security.md](security.md) | security review follow-ups that change behaviour or need a design |
| [programs.md](programs.md) | programs driving Longarm: features, fixtures, session input (docs/programs.md) |