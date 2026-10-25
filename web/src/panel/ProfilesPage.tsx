import { useState } from 'react'
import { hubApi } from '../api/hub'
import type { Profile, ToolSpec } from '../api/types'
import { ErrorBox, StatePill, useAction, usePoll } from '../components/common'
import { fmtTime } from '../lib/format'

const KNOWN = 'git gh node pnpm uv pwsh claude copilot go'

export function parseTools(text: string): ToolSpec[] {
  return text
    .split(/[\n,]+/)
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => {
      const [name, version] = l.split(/\s*[=@ ]\s*/, 2)
      return { name: name.trim(), version: (version ?? 'latest').trim() || 'latest' }
    })
}

export function formatTools(tools?: ToolSpec[]): string {
  return (tools ?? []).map((t) => `${t.name}=${t.version}`).join('\n')
}

/** Toolchain profiles: view / edit, rollout (canary) status per profile. */
export function ProfilesPage() {
  const profiles = usePoll(hubApi.profiles, 5000)
  const nodes = usePoll(() => hubApi.nodes(false), 5000)
  const [editing, setEditing] = useState<{ name: string; text: string; isNew: boolean }>()
  const { run, busy } = useAction()
  return (
    <div>
      <div className="page-title">
        <h1>Toolchain profiles</h1>
        <span className="spacer" />
        <button className="primary" onClick={() => setEditing({ name: '', text: 'git=latest\nnode=22\npnpm=latest', isNew: true })}>
          New profile
        </button>
      </div>
      <ErrorBox error={profiles.error} />
      {editing && (
        <form
          className="card"
          style={{ marginBottom: 12 }}
          onSubmit={async (e) => {
            e.preventDefault()
            const r = await run(() => hubApi.putProfile(editing.name, parseTools(editing.text)), (p: Profile) => `${p.name} r${p.revision} saved`)
            if (r) {
              setEditing(undefined)
              profiles.reload()
            }
          }}
        >
          <div className="form-grid">
            <label>name</label>
            <input aria-label="profile name" value={editing.name} disabled={!editing.isNew} onChange={(e) => setEditing({ ...editing, name: e.target.value })} />
            <label>tools</label>
            <textarea aria-label="tools" rows={8} value={editing.text} onChange={(e) => setEditing({ ...editing, text: e.target.value })} />
          </div>
          <p className="muted small">
            One <span className="mono">tool=version</span> per line (version prefix or <span className="mono">latest</span>). Known tools: {KNOWN}. A new revision goes to one canary worker first, then to the rest. CLI: <span className="mono">longarm admin profile set</span>.
          </p>
          <button className="primary" disabled={busy || !editing.name}>
            Save
          </button>{' '}
          <button type="button" onClick={() => setEditing(undefined)}>
            Cancel
          </button>
        </form>
      )}
      <div className="cards">
        {(profiles.data ?? []).map((p) => {
          const assigned = (nodes.data ?? []).filter((n) => n.profile === p.name)
          return (
            <div className="card" key={p.name} data-testid={`profile-${p.name}`}>
              <div className="row">
                <b>{p.name}</b>
                <span className="muted">r{p.revision}</span>
                <StatePill state={p.rollout} />
                {p.canary && <span className="muted small">canary {p.canary}</span>}
                <span className="spacer" />
                <button onClick={() => setEditing({ name: p.name, text: formatTools(p.tools), isNew: false })}>Edit</button>
              </div>
              {p.message && <div className="small amber">{p.message}</div>}
              <div className="codebox small" style={{ marginTop: 8 }}>
                {formatTools(p.tools)}
              </div>
              <div className="small muted" style={{ marginTop: 6 }}>
                updated {fmtTime(p.updated_at)}
              </div>
              <table className="grid small" style={{ marginTop: 6 }}>
                <tbody>
                  {assigned.map((n) => (
                    <tr key={n.name}>
                      <td>{n.name}</td>
                      <td>r{n.toolchain?.revision ?? '—'}</td>
                      <td>
                        <StatePill state={n.toolchain?.state} />
                      </td>
                      <td className="mono" title={n.toolchain?.error}>
                        {Object.entries(n.toolchain?.versions ?? {})
                          .map(([k, v]) => `${k} ${v || '✗'}`)
                          .join(' · ')}
                      </td>
                    </tr>
                  ))}
                  {assigned.length === 0 && (
                    <tr>
                      <td className="faint">no workers assigned (Nodes → toolchain)</td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
          )
        })}
      </div>
    </div>
  )
}
