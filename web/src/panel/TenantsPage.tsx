import { useState } from 'react'
import { hubApi } from '../api/hub'
import type { Me, PutUserRequest, PutUserResponse, Quota, Team, TenantUser } from '../api/types'
import { ErrorBox, useAction, usePoll } from '../components/common'
import { fmtTime } from '../lib/format'
import { QUOTA_LABELS, UsageOf } from './MePage'

type QuotaForm = Partial<Record<keyof Quota, string>>

const INT_FIELDS = new Set<keyof Quota>(['max_jobs', 'max_jobs_per_worker', 'max_sessions_per_worker'])

const TTLS: [string, string][] = [
  ['3600s', '1 hour'],
  ['86400s', '1 day'],
  ['604800s', '7 days'],
]

const splitList = (s: string) => s.split(/[\s,]+/).filter(Boolean)

function quotaForm(q?: Quota): QuotaForm {
  const f: QuotaForm = {}
  for (const [k] of QUOTA_LABELS) if ((q?.[k] ?? 0) > 0) f[k] = String(q?.[k])
  return f
}

/** The form's quota fields (empty = the hub's default); a string is an error. */
function parseQuota(f: QuotaForm): Quota | string {
  const q: Quota = {}
  for (const [k, label] of QUOTA_LABELS) {
    const s = (f[k] ?? '').trim()
    if (!s) continue
    const n = Number(s)
    if (!Number.isFinite(n) || n < 0 || (INT_FIELDS.has(k) && !Number.isInteger(n))) return `${label}: not a ${INT_FIELDS.has(k) ? 'whole ' : ''}number`
    if (n > 0) q[k] = n
  }
  return q
}

const quotaEmpty = (q?: Quota) => !q || QUOTA_LABELS.every(([k]) => !q[k])

/** The user's quota with the hub's default for unset fields. */
function effective(u: TenantUser, def?: Quota): Quota {
  const q: Quota = {}
  for (const [k] of QUOTA_LABELS) q[k] = u.quota?.[k] || def?.[k] || undefined
  return q
}

function fmtQuota(q?: Quota): string {
  return QUOTA_LABELS.filter(([k]) => (q?.[k] ?? 0) > 0)
    .map(([k, label, unit]) => `${label} ${q?.[k]}${unit ? ' ' + unit : ''}`)
    .join(' · ')
}

/** What PutUser handed out once: a device join code and / or a panel login. */
interface Issued {
  user: string
  res: PutUserResponse
  install?: { windows: string; linux: string }
}

/**
 * People sharing the hub (Longarm users, multi-tenancy): their devices,
 * teams, quotas and usage. Like longarm admin user | team.
 */
export function TenantsPage({ me }: { me: Me }) {
  const tenants = usePoll(hubApi.tenants, 15000)
  const teams = usePoll(hubApi.teams, 15000)
  const [adding, setAdding] = useState(false)
  const [open, setOpen] = useState<{ name: string; mode: 'edit' | 'remove' }>()
  const [issued, setIssued] = useState<Issued>()
  const users = tenants.data?.users ?? []
  const def = tenants.data?.default_quota
  const reload = () => {
    tenants.reload()
    teams.reload()
  }
  const onIssued = async (user: string, res: PutUserResponse) => {
    const jc = res.join_code
    const install = jc?.code ? await hubApi.install(jc.code, jc.role || 'client').catch(() => undefined) : undefined
    if (jc?.code || res.panel_password) setIssued({ user, res, install })
    reload()
  }
  return (
    <div>
      <div className="page-title">
        <h1>People</h1>
        {tenants.data && (
          <span
            className={`pill ${tenants.data.tenancy ? 'ok' : ''}`}
            data-testid="tenancy"
            title={tenants.data.tenancy ? 'users only see and control their own jobs, sessions, files and forwards' : 'off until the first user exists (or a worker isolates users)'}
          >
            multi-tenancy {tenants.data.tenancy ? 'on' : 'off'}
          </span>
        )}
        <span className="spacer" />
        <button className="primary" onClick={() => setAdding((v) => !v)}>
          Add user
        </button>
      </div>
      <p className="muted small" data-testid="default-quota">
        Default quota: {quotaEmpty(def) ? 'no limits' : fmtQuota(def)} <span className="faint">(hub config [tenancy] default_quota)</span>
      </p>
      <ErrorBox error={tenants.error} />
      {adding && (
        <AddUser
          onDone={(user, res) => {
            setAdding(false)
            onIssued(user, res)
          }}
        />
      )}
      {issued && <IssuedCard issued={issued} me={me} onClose={() => setIssued(undefined)} />}
      <table className="grid" data-testid="tenants">
        <thead>
          <tr>
            <th>user</th>
            <th>teams</th>
            <th>devices</th>
            <th>active jobs</th>
            <th>reserved memory</th>
            <th>quota overrides</th>
            <th>panel login</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <TenantRows
              key={u.name}
              u={u}
              def={def}
              open={open?.name === u.name ? open.mode : undefined}
              setOpen={(mode) => setOpen(mode ? { name: u.name, mode } : undefined)}
              onIssued={onIssued}
              reload={reload}
            />
          ))}
          {tenants.data && users.length === 0 && (
            <tr>
              <td colSpan={8} className="notice">
                No users yet: everybody acts as an administrator. Add a user per person to separate their jobs and data.
              </td>
            </tr>
          )}
        </tbody>
      </table>
      <Teams teams={teams.data ?? []} error={teams.error} users={users.map((u) => u.name)} reload={reload} setTeams={teams.setData} />
      <p className="muted small" style={{ marginTop: 16 }}>
        CLI: <span className="mono">longarm admin user add | set | code | ls | remove</span>, <span className="mono">longarm admin team</span>.
      </p>
    </div>
  )
}

function TenantRows({
  u,
  def,
  open,
  setOpen,
  onIssued,
  reload,
}: {
  u: TenantUser
  def?: Quota
  open?: 'edit' | 'remove'
  setOpen: (mode?: 'edit' | 'remove') => void
  onIssued: (user: string, res: PutUserResponse) => void
  reload: () => void
}) {
  const { run, busy } = useAction()
  const eff = effective(u, def)
  const newCode = async () => {
    const device = prompt(`Device name for ${u.name} (empty: automatic):`, '')
    if (device === null) return
    const r = await run(() => hubApi.putTenant({ name: u.name, join_code: true, ttl: '86400s', device: device.trim() || undefined }))
    if (r) onIssued(u.name, r)
  }
  return (
    <>
      <tr data-testid={`tenant-${u.name}`} style={u.disabled ? { opacity: 0.6 } : undefined}>
        <td className="nowrap">
          <b>{u.name}</b> {u.disabled && <span className="pill bad">disabled</span>}
          <div className="muted small">
            created {fmtTime(u.created_at)}
            {u.created_by ? ` by ${u.created_by}` : ''}
          </div>
        </td>
        <td className="small">
          {(u.teams ?? []).map((t) => (
            <span key={t} className="pill" style={{ marginRight: 3 }}>
              {t}
            </span>
          ))}
        </td>
        <td className="small">{(u.devices ?? []).length ? (u.devices ?? []).join(', ') : <span className="faint">none</span>}</td>
        <td>
          <UsageOf used={u.usage?.jobs ?? 0} limit={eff.max_jobs} />
        </td>
        <td>
          <UsageOf used={u.usage?.mem_gb ?? 0} limit={eff.max_mem_gb} unit="GB" />
        </td>
        <td className="small muted">{quotaEmpty(u.quota) ? <span className="faint">default</span> : fmtQuota(u.quota)}</td>
        <td>{u.panel ? 'yes' : <span className="faint">—</span>}</td>
        <td className="nowrap" style={{ textAlign: 'right' }}>
          <button disabled={busy} onClick={() => setOpen(open === 'edit' ? undefined : 'edit')}>
            Edit
          </button>{' '}
          <button disabled={busy || u.disabled} title="a one-time client join code for another device of this user" onClick={newCode}>
            New device code
          </button>{' '}
          <button className="danger" disabled={busy} onClick={() => setOpen(open === 'remove' ? undefined : 'remove')}>
            Remove
          </button>
        </td>
      </tr>
      {open === 'edit' && (
        <tr>
          <td colSpan={8}>
            <EditUser
              u={u}
              onDone={(res) => {
                setOpen(undefined)
                onIssued(u.name, res)
              }}
              onCancel={() => setOpen(undefined)}
            />
          </td>
        </tr>
      )}
      {open === 'remove' && (
        <tr>
          <td colSpan={8}>
            <RemoveUser
              u={u}
              onDone={() => {
                setOpen(undefined)
                reload()
              }}
              onCancel={() => setOpen(undefined)}
            />
          </td>
        </tr>
      )}
    </>
  )
}

function QuotaFields({ value, onChange }: { value: QuotaForm; onChange: (f: QuotaForm) => void }) {
  return (
    <div className="form-grid small">
      {QUOTA_LABELS.map(([k, label, unit]) => (
        <QuotaField key={k} label={label} unit={unit} value={value[k] ?? ''} onChange={(v) => onChange({ ...value, [k]: v })} />
      ))}
    </div>
  )
}

function QuotaField({ label, unit, value, onChange }: { label: string; unit: string; value: string; onChange: (v: string) => void }) {
  return (
    <>
      <label>
        {label}
        {unit ? ` (${unit})` : ''}
      </label>
      <input aria-label={`quota ${label}`} inputMode="decimal" placeholder="default" value={value} onChange={(e) => onChange(e.target.value)} />
    </>
  )
}

function AddUser({ onDone }: { onDone: (user: string, res: PutUserResponse) => void }) {
  const { run, busy } = useAction()
  const [name, setName] = useState('')
  const [teams, setTeams] = useState('')
  const [quota, setQuota] = useState<QuotaForm>({})
  const [joinCode, setJoinCode] = useState(true)
  const [ttl, setTtl] = useState('86400s')
  const [admin, setAdmin] = useState(false)
  const [panel, setPanel] = useState(false)
  const [err, setErr] = useState<string>()
  return (
    <form
      className="card"
      style={{ marginBottom: 12 }}
      data-testid="add-user"
      onSubmit={async (e) => {
        e.preventDefault()
        const q = parseQuota(quota)
        if (typeof q === 'string') return setErr(q)
        setErr(undefined)
        const req: PutUserRequest = { name: name.trim(), create: true, set_teams: true, teams: splitList(teams), join_code: joinCode, panel }
        if (!quotaEmpty(q)) {
          req.set_quota = true
          req.quota = q
        }
        if (joinCode) {
          req.ttl = ttl
          req.admin = admin
        }
        const r = await run(() => hubApi.putTenant(req), `user ${req.name} added`)
        if (r) onDone(req.name, r)
      }}
    >
      <div className="form-grid">
        <label>name</label>
        <input aria-label="user name" placeholder="a-z 0-9 _ -, e.g. alice" value={name} onChange={(e) => setName(e.target.value)} />
        <label>teams</label>
        <input aria-label="teams" placeholder="comma separated, optional" value={teams} onChange={(e) => setTeams(e.target.value)} />
      </div>
      <details style={{ margin: '10px 0' }}>
        <summary className="muted">quota (optional; empty fields use the default)</summary>
        <div style={{ marginTop: 8 }}>
          <QuotaFields value={quota} onChange={setQuota} />
        </div>
      </details>
      <div>
        <label>
          <input type="checkbox" checked={joinCode} onChange={(e) => setJoinCode(e.target.checked)} /> issue a join code for the first device
        </label>{' '}
        <select aria-label="join code ttl" value={ttl} disabled={!joinCode} onChange={(e) => setTtl(e.target.value)}>
          {TTLS.map(([v, l]) => (
            <option key={v} value={v}>
              valid {l}
            </option>
          ))}
        </select>
      </div>
      <div>
        <label title="the device gets an admin certificate (longarm admin ... works there)">
          <input type="checkbox" checked={admin} disabled={!joinCode} onChange={(e) => setAdmin(e.target.checked)} /> admin certificate
        </label>
      </div>
      <div>
        <label title="a login (role user) that sees only this user's jobs; password and TOTP secret are shown once">
          <input type="checkbox" checked={panel} onChange={(e) => setPanel(e.target.checked)} /> web panel login
        </label>
      </div>
      <ErrorBox error={err} />
      <p>
        <button className="primary" disabled={busy || !name.trim()}>
          Create user
        </button>
      </p>
    </form>
  )
}

function EditUser({ u, onDone, onCancel }: { u: TenantUser; onDone: (res: PutUserResponse) => void; onCancel: () => void }) {
  const { run, busy } = useAction()
  const [teams, setTeams] = useState((u.teams ?? []).join(', '))
  const [quota, setQuota] = useState<QuotaForm>(quotaForm(u.quota))
  const [disabled, setDisabled] = useState(!!u.disabled)
  const [panel, setPanel] = useState(false)
  const [err, setErr] = useState<string>()
  return (
    <form
      data-testid={`edit-${u.name}`}
      onSubmit={async (e) => {
        e.preventDefault()
        const q = parseQuota(quota)
        if (typeof q === 'string') return setErr(q)
        setErr(undefined)
        const req: PutUserRequest = { name: u.name, set_teams: true, teams: splitList(teams), set_quota: true, quota: quotaEmpty(q) ? undefined : q, set_disabled: true, disabled, panel }
        const r = await run(() => hubApi.putTenant(req), `${u.name} saved`)
        if (r) onDone(r)
      }}
    >
      <div className="form-grid">
        <label>teams</label>
        <input aria-label={`teams of ${u.name}`} placeholder="comma separated" value={teams} onChange={(e) => setTeams(e.target.value)} />
      </div>
      <div className="muted small" style={{ margin: '10px 0 4px' }}>
        quota overrides (empty: the hub's default)
      </div>
      <QuotaFields value={quota} onChange={setQuota} />
      <div style={{ marginTop: 8 }}>
        <label title="calls of the user's devices are refused until enabled again">
          <input type="checkbox" checked={disabled} onChange={(e) => setDisabled(e.target.checked)} /> disabled
        </label>
      </div>
      <div>
        <label title="new password and TOTP secret, shown once; ends the login's sessions">
          <input type="checkbox" checked={panel} onChange={(e) => setPanel(e.target.checked)} /> {u.panel ? 'reset the web panel login' : 'create a web panel login'}
        </label>
      </div>
      <ErrorBox error={err} />
      <p>
        <button className="primary" disabled={busy}>
          Save
        </button>{' '}
        <button type="button" onClick={onCancel}>
          Cancel
        </button>
      </p>
    </form>
  )
}

function RemoveUser({ u, onDone, onCancel }: { u: TenantUser; onDone: () => void; onCancel: () => void }) {
  const { run, busy } = useAction()
  const [purge, setPurge] = useState(false)
  return (
    <div className="row wrap" data-testid={`remove-${u.name}`}>
      <span>
        Remove <b>{u.name}</b>? Revokes the certificates of {(u.devices ?? []).length ? (u.devices ?? []).join(', ') : 'all devices'} and deletes the panel login.
      </span>
      <label>
        <input type="checkbox" checked={purge} onChange={(e) => setPurge(e.target.checked)} /> purge data and OS account on workers
      </label>
      <span className="spacer" />
      <button
        className="danger"
        disabled={busy}
        onClick={async () => {
          const r = await run(
            () => hubApi.removeTenant(u.name, purge),
            (r) => `${u.name} removed${r.revoked?.length ? `, revoked ${r.revoked.join(', ')}` : ''}${r.purge ? ', purging on workers' : ''}`,
          )
          if (r) onDone()
        }}
      >
        Remove {u.name}
      </button>
      <button onClick={onCancel}>Cancel</button>
    </div>
  )
}

/** A new device's join code (with install one-liners) and / or panel credentials, shown once. */
function IssuedCard({ issued, me, onClose }: { issued: Issued; me: Me; onClose: () => void }) {
  const { res, install, user } = issued
  const jc = res.join_code
  return (
    <div className="cards" style={{ marginBottom: 12 }}>
      {jc?.code && (
        <div className="card" data-testid="tenant-code">
          <div className="muted small">
            one-time {jc.role} code for a device of <b>{user}</b>
            {jc.name ? ` (node ${jc.name})` : ''}, valid until {fmtTime(jc.expires_at)}
          </div>
          <div className="bigcode" data-testid="join-code">
            {jc.code}
          </div>
          <p className="muted small">On {user}'s machine, enter it in longarm-tray (“Join a fleet”) or run one of these:</p>
          <div className="small muted">Windows (PowerShell)</div>
          <div className="codebox" data-testid="install-windows">
            {install?.windows}
          </div>
          <div className="small muted" style={{ marginTop: 8 }}>
            Linux / macOS
          </div>
          <div className="codebox" data-testid="install-linux">
            {install?.linux}
          </div>
          <div className="small muted" style={{ marginTop: 8 }}>
            Already installed:{' '}
            <span className="mono">
              longarm join {me.hub_address} {jc.code} --ca-fingerprint {me.ca_fingerprint}
            </span>
          </div>
        </div>
      )}
      {res.panel_password && (
        <div className="card" data-testid="panel-login">
          <p className="small">
            Web panel login for <b>{user}</b>; hand these over now, they are not shown again.
          </p>
          <div className="small muted">password</div>
          <div className="codebox" data-testid="panel-password">
            {res.panel_password}
          </div>
          <div className="small muted" style={{ marginTop: 8 }}>
            TOTP secret (add to an authenticator app)
          </div>
          <div className="codebox" data-testid="panel-totp">
            {res.totp_secret}
          </div>
          <div className="codebox small" style={{ marginTop: 6 }}>
            {res.otpauth}
          </div>
        </div>
      )}
      <div>
        <button onClick={onClose}>Done</button>
      </div>
    </div>
  )
}

/** Teams exist through their members: adding a member to a new name creates the team. */
function Teams({ teams, error, users, reload, setTeams }: { teams: Team[]; error?: string; users: string[]; reload: () => void; setTeams: (t: Team[]) => void }) {
  const { run, busy } = useAction()
  const [newTeam, setNewTeam] = useState('')
  const [newMember, setNewMember] = useState('')
  const change = async (team: string, add: string[], remove: string[], ok: string) => {
    const r = await run(() => hubApi.setTeam(team, add, remove), ok)
    if (r) {
      setTeams(r)
      reload()
    }
    return r
  }
  return (
    <>
      <h3 className="muted" style={{ marginTop: 24 }}>
        Teams
      </h3>
      <p className="muted small">Members may read jobs shared with the team (longarm run --share team); they cannot control them.</p>
      <ErrorBox error={error} />
      <table className="grid" data-testid="teams">
        <tbody>
          {teams.map((t) => (
            <TeamRow key={t.name} t={t} users={users} busy={busy} change={change} />
          ))}
          <tr>
            <td>
              <input aria-label="new team" placeholder="new team" value={newTeam} onChange={(e) => setNewTeam(e.target.value)} />
            </td>
            <td>
              <select aria-label="new team member" value={newMember} onChange={(e) => setNewMember(e.target.value)}>
                <option value="">first member…</option>
                {users.map((u) => (
                  <option key={u}>{u}</option>
                ))}
              </select>
            </td>
            <td style={{ textAlign: 'right' }}>
              <button
                disabled={busy || !newTeam.trim() || !newMember}
                onClick={async () => {
                  if (await change(newTeam.trim(), [newMember], [], `team ${newTeam.trim()} created`)) {
                    setNewTeam('')
                    setNewMember('')
                  }
                }}
              >
                Create team
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </>
  )
}

function TeamRow({ t, users, busy, change }: { t: Team; users: string[]; busy: boolean; change: (team: string, add: string[], remove: string[], ok: string) => Promise<unknown> }) {
  const [add, setAdd] = useState('')
  const members = t.members ?? []
  return (
    <tr data-testid={`team-${t.name}`}>
      <td>
        <b>{t.name}</b>
      </td>
      <td className="small">
        {members.map((m) => (
          <span key={m} className="pill" style={{ marginRight: 3 }}>
            {m}{' '}
            <button className="link small" title={`remove ${m} from ${t.name}`} aria-label={`remove ${m} from ${t.name}`} disabled={busy} onClick={() => change(t.name, [], [m], `${m} left ${t.name}`)}>
              ×
            </button>
          </span>
        ))}
      </td>
      <td className="nowrap" style={{ textAlign: 'right' }}>
        <select aria-label={`add member to ${t.name}`} value={add} onChange={(e) => setAdd(e.target.value)}>
          <option value="">add member…</option>
          {users
            .filter((u) => !members.includes(u))
            .map((u) => (
              <option key={u}>{u}</option>
            ))}
        </select>{' '}
        <button disabled={busy || !add} onClick={() => change(t.name, [add], [], `${add} joined ${t.name}`).then(() => setAdd(''))}>
          Add
        </button>
      </td>
    </tr>
  )
}
