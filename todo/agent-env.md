# Agent environment of runs (`--agent-env`)

docs/usage.md "Agent environment (--agent-env)"; cmd/longarm/agentenv.go.

## The tray and the MCP tools

`run` / `exec` / `session start` / `attach` take `--agent-env`, `--mcp`,
`--skill`, `--plugin`; the tray's new-session dialog
(`daemon_agents.go`, the `forwardAgentAuth` call) and the MCP tools
`fleet_exec` / `fleet_job_submit` (`mcp.go`) don't. Add an `agent_env`
field next to `agent_auth` and call `forwardAgentEnv` there; for MCP, an
allowlist like `LONGARM_MCP_AGENT_AUTH` (MCP configurations carry
credentials).

## A default in workers.toml

Nothing goes unless asked. `[agent_env] default = "skills,plugins"` (like
`[agent_auth] default`) would let a user always give remote agents their
skills; read it in `forwardAgentEnv` when no flag is given.
