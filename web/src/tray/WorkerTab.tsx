import { useCallback, useEffect, useState } from 'react'
import { longarmdApi, trayApi, traySettingsApi, type TrayInfo } from '../api/local'
import type { Job } from '../api/types'
import { Bar } from '../components/charts'
import { ErrorBox, FirewallWarning, LogView, StatePill, useAction, usePoll } from '../components/common'
import { IconChip, IconTerminal } from '../components/icons'
import { fmtAgo, fmtTime, gb, isActive, memPct, since } from '../lib/format'
import { killQuestion } from './ClientTab'
import { Banner, EmptyState, Section, Skeleton } from './ui'

/** longarm-tray's worker tab: this machine's longarmd through its local API. */
export function WorkerTab({ info, onNavigate }: { info: TrayInfo; onNavigate?: (tab: string) => void }) {
  if (!info.worker.enrolled) {
    // installed for a hub (the MSI's Default): signing in enrolls the worker; join codes are the exception
    if (info.account?.hub_from)
      return (
        <div data-testid="worker-signin">
          <EmptyState
            icon={<IconChip size={20} />}
            title="This worker isn't signed in yet"
            action={
              onNavigate && (
                <button className="primary" onClick={() => onNavigate('account')}>
                  Account tab
                </button>
              )
            }
          >
            Sign in on the Account tab and it joins {info.account.hub || 'the hub'} as yours.
          </EmptyState>
          <details className="disclosure">
            <summary>Use a join code</summary>
            <JoinForm />
          </details>
        </div>
      )
    return <JoinForm />
  }
  if (!info.worker.running) return <WorkerDown info={info} />
  return <WorkerStatusView />
}

/** The local worker isn't running: start it the way it runs, or look at its log. */
function WorkerDown({ info }: { info: TrayInfo }) {
  const { run, busy } = useAction()
  const session = info.account?.run_mode === 'session'
  return (
    <div data-testid="worker-down">
      <Banner tone="error">
        longarmd is not running{info.worker.error ? `: ${info.worker.error}` : ''}.{' '}
        {session ? (
          <>
            It starts at your logon (<span className="mono">longarmd session-mode start</span>)
            {info.account?.needed ? '; it waits for your sign-in (Account tab)' : ''}.
          </>
        ) : (
          <>
            It runs as a service (<span className="mono">longarmd service start</span>; starting it may ask for an administrator).
          </>
        )}
      </Banner>
      <div className="button-row">
        <button className="primary" disabled={busy} onClick={() => run(() => traySettingsApi.startWorker(), session ? 'starting the worker' : 'starting the worker service')}>
          Start worker
        </button>
        <button disabled={busy} onClick={() => run(() => traySettingsApi.openFolder('worker-logs'))}>
          Open log folder
        </button>
      </div>
    </div>
  )
}

function WorkerStatusView() {
  const st = usePoll(longarmdApi.status, 2000)
  const jobs = usePoll(() => longarmdApi.jobs(30), 3000)
  const { run, busy } = useAction()
  const [open, setOpen] = useState<Job>()
  const [maxJobs, setMaxJobs] = useState('')
  const [reserve, setReserve] = useState('')
  const s = st.data
  useEffect(() => {
    if (s?.limits && maxJobs === '' && reserve === '') {
      setMaxJobs(String(s.limits.max_jobs ?? 0))
      setReserve(String(s.limits.reserved_mem_gb ?? 0))
    }
  }, [s]) // eslint-disable-line react-hooks/exhaustive-deps
  if (!s) return st.error ? <ErrorBox error={st.error} /> : <Skeleton card lines={5} label="loading this worker…" />
  const draining = !!s.state?.draining
  const m = s.metrics
  const hubOk = s.hub?.connected || !s.hub?.address
  return (
    <div>
      <ErrorBox error={st.error} />
      <FirewallWarning fw={s.firewall} />
      <section className="wcard">
        <div className="head">
          <span className={`dot ${hubOk ? 'is-online' : 'is-offline'}`} style={{ background: hubOk ? 'var(--green)' : 'var(--red)' }} />
          <span className="name">{s.name}</span>
          <span className="sub">
            longarmd {s.version} · {s.os}/{s.arch}
          </span>
          <span className="actions">
            <button
              data-testid="drain-toggle"
              className="act"
              disabled={busy}
              onClick={() => run(() => longarmdApi.drain(!draining, draining ? '' : 'paused in longarm-tray'), draining ? 'accepting work' : 'draining').then(st.reload)}
            >
              {draining ? 'resume' : 'pause (drain)'}
            </button>
          </span>
        </div>
        <div className="small state-line" data-testid="worker-state">
          {draining ? <span className="pill warn">draining: {s.state?.reason}</span> : <span className="pill ok">accepting work</span>}{' '}
          <span className="muted">
            · {s.jobs_running ?? 0} jobs, {s.sessions_running ?? 0} sessions
          </span>
        </div>
        <dl className="facts small">
          <dt>hub</dt>
          <dd>
            <span className="mono">{s.hub?.address || 'not configured'}</span>{' '}
            {s.hub?.address && (
              <span className="muted">
                {s.hub.connected ? `connected (${s.hub.node_state}) ${fmtAgo(since(s.hub.since))}` : `disconnected ${s.hub.error ?? ''}`}
              </span>
            )}
          </dd>
          <dt>listening</dt>
          <dd className="mono">{(s.endpoints ?? []).join(' ')}</dd>
          {s.cert_not_after && (
            <>
              <dt>certificate</dt>
              <dd>valid until {fmtTime(s.cert_not_after)}</dd>
            </>
          )}
        </dl>
        {m && (
          <div style={{ marginTop: 6 }}>
            <Bar label="CPU" pct={m.cpu_percent ?? 0} value={`${(m.cpu_percent ?? 0).toFixed(0)}%`} />
            <Bar label="MEM" pct={memPct(m)} value={`${gb(m.mem_used_bytes).toFixed(1)} / ${gb(m.mem_total_bytes).toFixed(0)} GB`} />
          </div>
        )}
      </section>
      <Section title="Local limits">
        <form
          className="limits-form"
          onSubmit={(e) => {
            e.preventDefault()
            run(() => longarmdApi.limits({ max_jobs: Number(maxJobs) || 0, reserved_mem_gb: Number(reserve) || 0 }), 'limits saved').then(st.reload)
          }}
        >
          <label className="field">
            <span className="field-label">max jobs</span>
            <input aria-label="max jobs" type="number" min={0} value={maxJobs} onChange={(e) => setMaxJobs(e.target.value)} />
          </label>
          <label className="field">
            <span className="field-label">keep free (GB)</span>
            <input aria-label="reserved memory" type="number" min={0} step="0.5" value={reserve} onChange={(e) => setReserve(e.target.value)} />
          </label>
          <button disabled={busy}>Save</button>
        </form>
        <div className="field-help">0 = no limit. The hub&apos;s scheduler honours them (longarmd limits).</div>
      </Section>
      <Section
        title="Toolchain"
        aside={
          <button className="sm" disabled={busy} onClick={() => run(() => longarmdApi.toolchainApply(), 'toolchain update requested')}>
            Update now
          </button>
        }
      >
        <div className="row small">
          {s.toolchain && s.toolchain.state !== 'none' ? (
            <>
              <span>
                {s.toolchain.profile} r{s.toolchain.revision}
              </span>
              <StatePill state={s.toolchain.state} />
              {s.toolchain.error && <span className="pill bad">{s.toolchain.error}</span>}
            </>
          ) : (
            <span className="muted">no profile assigned</span>
          )}
        </div>
        <div className="small mono muted">
          {Object.entries(s.toolchain?.versions ?? {})
            .map(([k, v]) => `${k} ${v || '✗'}`)
            .join(' · ')}
        </div>
      </Section>
      <Section title="Jobs on this worker">
        <ErrorBox error={jobs.error} />
        <table className="grid list small" data-testid="worker-jobs">
          <tbody>
            {(jobs.data ?? []).map((j) => (
              <tr
                key={j.spec.id}
                className={`clickable ${open?.spec.id === j.spec.id ? 'selected' : ''}`}
                tabIndex={0}
                onClick={() => setOpen(j)}
                onKeyDown={(e) => {
                  if ((e.key === 'Enter' || e.key === ' ') && e.target === e.currentTarget) {
                    e.preventDefault()
                    setOpen(j)
                  }
                }}
              >
                <td className="mono strong">{j.spec.name || j.spec.id.slice(-8)}</td>
                <td>
                  <StatePill state={j.state} />
                </td>
                <td className="muted">{j.spec.owner || j.spec.submitted_by}</td>
                <td className="muted nowrap hide-narrow">{fmtTime(j.created_at)}</td>
                <td className="actions-cell">
                  {isActive(j.state) && (
                    <button
                      className="danger sm"
                      disabled={busy}
                      onClick={(e) => {
                        e.stopPropagation()
                        if (!confirm(killQuestion(j, s.name))) return
                        run(() => longarmdApi.kill(j.spec.id), 'killed').then(jobs.reload)
                      }}
                    >
                      kill
                    </button>
                  )}
                </td>
              </tr>
            ))}
            {jobs.data === undefined && !jobs.error && (
              <tr className="placeholder">
                <td colSpan={5}>
                  <Skeleton lines={2} label="loading jobs…" />
                </td>
              </tr>
            )}
            {jobs.data !== undefined && jobs.data.length === 0 && (
              <tr className="placeholder">
                <td colSpan={5}>
                  <EmptyState compact icon={<IconTerminal size={20} />} title="No jobs on this worker yet" />
                </td>
              </tr>
            )}
          </tbody>
        </table>
        {open && <WorkerJobLogs key={open.spec.id} id={open.spec.id} />}
      </Section>
      <Section title="Fleet membership">
        <div className="field-inline">
          <div className="field-help">The hub revokes this worker and longarmd stops. Running jobs keep running.</div>
          <button
            className="danger"
            disabled={busy}
            onClick={() =>
              confirm('Leave the fleet? The hub revokes this worker and longarmd stops. Running jobs keep running.') &&
              run(() => longarmdApi.leave('left in longarm-tray'), (r) => r.message ?? 'left')
            }
          >
            Leave the fleet
          </button>
        </div>
      </Section>
    </div>
  )
}

function WorkerJobLogs({ id }: { id: string }) {
  const open = useCallback((onChunk: Parameters<typeof longarmdApi.logs>[1], onDone: Parameters<typeof longarmdApi.logs>[2]) => longarmdApi.logs(id, onChunk, onDone), [id])
  return <LogView open={open} />
}

/** Not enrolled yet: join with a code from the hub panel (longarmd join). */
function JoinForm() {
  const { run, busy } = useAction()
  const [hub, setHub] = useState('')
  const [code, setCode] = useState('')
  const [fp, setFp] = useState('')
  const [name, setName] = useState('')
  const [out, setOut] = useState<string>()
  return (
    <form
      className="wcard join-form"
      onSubmit={async (e) => {
        e.preventDefault()
        const r = await run(() => trayApi.join(hub, code, fp, name))
        if (r) setOut(r.output)
      }}
    >
      <h3 className="card-title">Join a fleet</h3>
      <div className="form-grid">
        <label htmlFor="join-hub">hub</label>
        <input id="join-hub" aria-label="hub address" placeholder="hub.example.com:7443" value={hub} onChange={(e) => setHub(e.target.value)} />
        <label htmlFor="join-code">join code</label>
        <input id="join-code" aria-label="join code" placeholder="K7P-Q2M" value={code} onChange={(e) => setCode(e.target.value)} />
        <label htmlFor="join-fp">CA fingerprint</label>
        <input id="join-fp" aria-label="CA fingerprint" placeholder="shown in the hub panel (recommended)" value={fp} onChange={(e) => setFp(e.target.value)} />
        <label htmlFor="join-name">name</label>
        <input id="join-name" aria-label="worker name" placeholder="default: hostname" value={name} onChange={(e) => setName(e.target.value)} />
      </div>
      <div className="button-row">
        <button className="primary" disabled={busy || !hub || !code}>
          Join
        </button>
      </div>
      {out && <pre className="logview">{out}</pre>}
      <p className="muted small">Runs longarmd join; the worker then waits for an admin&apos;s approval in the hub panel.</p>
    </form>
  )
}
