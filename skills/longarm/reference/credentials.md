# Credentials: agents, git, secrets

All forwarded values travel worker-direct over mTLS (never through the hub). They stay in the job's memory and environment and are redacted from `logs` and session transcripts. Listings show names only, never values.

## `--agent-auth` (run, exec, session start, attach-that-creates)

| value | forwards |
|---|---|
| `auto` (default) | the agent named by `argv[0]`'s base name (`claude`, `copilot`; `.exe/.cmd/.ps1/.js` stripped). Wrappers such as `pwsh -c`, `sh -c`, `npx` are not detected |
| `claude` | `env` block of `$CLAUDE_CONFIG_DIR`/`~/.claude/settings.json` (token-like names as secrets), plus `ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN` from the environment |
| `copilot` | as `COPILOT_GITHUB_TOKEN`: `--copilot-account NAME` (pool `~/.copilot/accounts/tokens.json`), else `COPILOT_GITHUB_TOKEN`/`GH_TOKEN`/`GITHUB_TOKEN`, else `gh auth token` |
| `gh` | `GH_TOKEN` with the token's full scope (opt-in only) |
| `none` | nothing |

- A Claude subscription-only login (no API key, proxy token or `CLAUDE_CODE_OAUTH_TOKEN`) forwards nothing. Tell the user to run `claude setup-token` and set `CLAUDE_CODE_OAUTH_TOKEN`.
- Copilot accepts `gho_` tokens and fine-grained PATs, not classic `ghp_` PATs. Copilot jobs get a private `COPILOT_HOME`. To use the account's `~/.copilot` instead, pass `-e COPILOT_HOME=...`.
- Which workers receive credentials:
  - Yours: yes.
  - Someone else's with per-user isolation: yes.
  - Someone else's with shared isolation: refused (exit 2) unless `--agent-auth-shared-ok`.
  - Admin (`-admin`) workers: only when the agent is named explicitly, and only your own admin worker.
- An agent named explicitly that has no credentials gives exit 2. With `auto` you get a stderr note and the job runs without them.
- Defaults live in workers.toml `[agent_auth] default = "none"|"auto"|"claude,copilot"`, `copilot_gh_user`, `gh_user`.
- MCP: `agent_auth` on `fleet_exec` / `fleet_job_submit`, limited to the agents in `LONGARM_MCP_AGENT_AUTH` (default `claude,copilot`).

## `--git-auth forward` (run/session start `--repo`, sync, handoff)

The worker fetches from the https remote with a short-lived credential obtained on this machine. The credential is used for that fetch only and is never visible to the job.

| host | source |
|---|---|
| Azure DevOps | `az account get-access-token` for the org's tenant (~1 h), else `git credential fill` |
| GitHub | `gh auth token` (long-lived), else `git credential fill` |
| other https | `git credential fill` |

- `--repo <url>`: no credential available here means exit 77 and nothing is submitted. If the remote refuses it, the job fails with 126.
- `--repo .` / sync: the worker fetches the base commits from the remote first, then only the unpushed delta is shipped. Any failure falls back to shipping everything, with a note.
- It is never the default: a credential leaves the machine only with this flag.
- Without it, a worker fetching a URL uses its own credentials without prompting. An interactive sign-in such as GCM for Azure DevOps fails fast with 401/403.

## Plain secrets

`--secret-env NAME` or `NAME=LOCALVAR` reads from longarm's own environment. It is memory-only on the worker and redacted. `-e K=V` is stored in job.json and shown by `jobs --json`, so never use it for secrets.
