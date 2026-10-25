import type { Me, Quota } from '../api/types'
import { MiniBar } from '../components/charts'

/** Quota fields in display order: [field, label, unit]. */
export const QUOTA_LABELS: [keyof Quota, string, string][] = [
  ['max_jobs', 'active jobs (fleet)', ''],
  ['max_mem_gb', 'reserved memory (fleet)', 'GB'],
  ['max_jobs_per_worker', 'jobs per worker', ''],
  ['max_mem_gb_per_worker', 'memory per worker', 'GB'],
  ['max_sessions_per_worker', 'sessions per worker', ''],
  ['job_mem_gb', 'default job memory', 'GB'],
  ['max_job_mem_gb', 'max job memory', 'GB'],
  ['job_cpus', 'default job CPUs', 'cores'],
  ['max_job_cpus', 'max job CPUs', 'cores'],
]

/** "3 / 8" style usage against a limit (no limit: just the value). */
export function UsageOf({ used, limit, unit }: { used: number; limit?: number; unit?: string }) {
  const u = unit ? ` ${unit}` : ''
  const shown = Number.isInteger(used) ? used : used.toFixed(1)
  if (!limit) return <span className="mono">{shown}{u}</span>
  return (
    <span className="mono nowrap">
      <MiniBar pct={(100 * used) / limit} /> {shown}/{limit}
      {u}
    </span>
  )
}

/** A panel login's own view: who it acts as, teams, quota and usage. */
export function MePage({ me }: { me: Me }) {
  const q = me.quota ?? {}
  const set = QUOTA_LABELS.filter(([k]) => (q[k] ?? 0) > 0)
  return (
    <div>
      <div className="page-title">
        <h1>Me</h1>
      </div>
      <div className="cards">
        <div className="card" data-testid="me">
          <div className="form-grid">
            <span className="muted">panel login</span>
            <span>
              <b>{me.user}</b> <span className="pill">{me.role ?? 'admin'}</span>
            </span>
            <span className="muted">acts as user</span>
            <span>{me.tenant || me.user}</span>
            <span className="muted">teams</span>
            <span data-testid="me-teams">
              {(me.teams ?? []).length === 0 && <span className="faint">none</span>}
              {(me.teams ?? []).map((t) => (
                <span key={t} className="pill" style={{ marginRight: 3 }}>
                  {t}
                </span>
              ))}
            </span>
            <span className="muted">multi-tenancy</span>
            <span>{me.tenancy ? 'on: users are separated' : 'off'}</span>
            <span className="muted">active jobs</span>
            <UsageOf used={me.usage?.jobs ?? 0} limit={q.max_jobs} />
            <span className="muted">reserved memory</span>
            <UsageOf used={me.usage?.mem_gb ?? 0} limit={q.max_mem_gb} unit="GB" />
          </div>
        </div>
        <div className="card" data-testid="me-quota">
          <h3 style={{ margin: '0 0 8px' }}>Quota</h3>
          {set.length === 0 && <div className="muted">No limits.</div>}
          <div className="form-grid small">
            {set.map(([k, label, unit]) => (
              <QuotaLine key={k} label={label} value={q[k]} unit={unit} />
            ))}
          </div>
        </div>
      </div>
      <p className="muted small">
        Your jobs run as your user on the workers; jobs shared with your teams are read-only. CLI: <span className="mono">longarm whoami</span>.
      </p>
    </div>
  )
}

function QuotaLine({ label, value, unit }: { label: string; value?: number; unit: string }) {
  return (
    <>
      <span className="muted">{label}</span>
      <span className="mono">
        {value}
        {unit ? ` ${unit}` : ''}
      </span>
    </>
  )
}
