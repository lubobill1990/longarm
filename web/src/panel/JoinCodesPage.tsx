import { useState } from 'react'
import { hubApi } from '../api/hub'
import type { JoinCode, Me } from '../api/types'
import { ErrorBox, StatePill, useAction, usePoll } from '../components/common'
import { fmtTime } from '../lib/format'

/** "Add machine": one-time join codes and the one-line install commands. */
export function JoinCodesPage({ me }: { me: Me }) {
  const codes = usePoll(hubApi.joinCodes, 15000)
  const profiles = usePoll(hubApi.profiles, 60000)
  const { run, busy } = useAction()
  const [role, setRole] = useState('worker')
  const [ttl, setTtl] = useState('10m')
  const [labels, setLabels] = useState('')
  const [profile, setProfile] = useState('')
  const [name, setName] = useState('')
  const [made, setMade] = useState<{ code: JoinCode; install?: { windows: string; linux: string } }>()
  return (
    <div>
      <div className="page-title">
        <h1>Add a machine</h1>
      </div>
      <div className="cards">
        <form
          className="card"
          onSubmit={async (e) => {
            e.preventDefault()
            const lab: Record<string, string> = {}
            for (const kv of labels.split(/[\s,]+/).filter(Boolean)) {
              const i = kv.indexOf('=')
              if (i > 0) lab[kv.slice(0, i)] = kv.slice(i + 1)
            }
            const code = await run(() => hubApi.createJoinCode({ role, ttl, labels: lab, profile, name }))
            if (code?.code) {
              const install = await hubApi.install(code.code, role).catch(() => undefined)
              setMade({ code, install })
              codes.reload()
            }
          }}
        >
          <div className="form-grid">
            <label>role</label>
            <select aria-label="role" value={role} onChange={(e) => setRole(e.target.value)}>
              <option value="worker">worker</option>
              <option value="client">client</option>
              <option value="admin">admin</option>
            </select>
            <label>valid for</label>
            <select aria-label="ttl" value={ttl} onChange={(e) => setTtl(e.target.value)}>
              <option value="10m">10 minutes</option>
              <option value="1h">1 hour</option>
              <option value="24h">1 day</option>
            </select>
            <label>labels</label>
            <input aria-label="labels" placeholder="region=eu pool=big" value={labels} onChange={(e) => setLabels(e.target.value)} />
            <label>toolchain</label>
            <select aria-label="profile" value={profile} onChange={(e) => setProfile(e.target.value)} disabled={role !== 'worker'}>
              <option value="">(none)</option>
              {(profiles.data ?? []).map((p) => (
                <option key={p.name}>{p.name}</option>
              ))}
            </select>
            <label>name</label>
            <input aria-label="node name" placeholder="default: the machine's hostname" value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <p>
            <button className="primary" disabled={busy}>
              Create join code
            </button>
          </p>
        </form>
        {made && (
          <div className="card" data-testid="new-code">
            <div className="muted small">one-time {made.code.role} code, valid until {fmtTime(made.code.expires_at)}</div>
            <div className="bigcode" data-testid="join-code">
              {made.code.code}
            </div>
            <p className="muted small">Enter it in longarm-tray (“Join a fleet”) on the new machine, or run one of these:</p>
            <div className="small muted">Windows (PowerShell)</div>
            <div className="codebox" data-testid="install-windows">
              {made.install?.windows}
            </div>
            <div className="small muted" style={{ marginTop: 8 }}>
              Linux / macOS
            </div>
            <div className="codebox" data-testid="install-linux">
              {made.install?.linux}
            </div>
            <div className="small muted" style={{ marginTop: 8 }}>
              Already installed: <span className="mono">{made.code.role === 'worker' ? 'longarmd' : 'longarm'} join {me.hub_address} {made.code.code} --ca-fingerprint {me.ca_fingerprint}</span>
            </div>
            {made.code.role === 'worker' && <p className="small muted">The new worker then waits under Nodes → “Waiting for approval”.</p>}
          </div>
        )}
      </div>
      <h3 className="muted" style={{ marginTop: 20 }}>
        Recent codes
      </h3>
      <ErrorBox error={codes.error} />
      <table className="grid">
        <thead>
          <tr>
            <th>id</th>
            <th>role</th>
            <th>name</th>
            <th>labels</th>
            <th>expires</th>
            <th>state</th>
            <th>by</th>
          </tr>
        </thead>
        <tbody>
          {(codes.data ?? []).map((c) => (
            <tr key={c.id}>
              <td className="mono">{c.id}</td>
              <td>{c.role}</td>
              <td>{c.name}</td>
              <td className="small">
                {Object.entries(c.labels ?? {})
                  .map(([k, v]) => `${k}=${v}`)
                  .join(' ')}
              </td>
              <td className="small">{fmtTime(c.expires_at)}</td>
              <td>{c.used ? <span className="muted">used by {c.used_by}</span> : <StatePill state={Date.parse(c.expires_at ?? '') < Date.now() ? 'expired' : 'pending'} />}</td>
              <td className="small muted">{c.created_by}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
