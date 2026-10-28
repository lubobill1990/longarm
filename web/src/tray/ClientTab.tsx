import { useCallback, useState } from 'react'
import { longarmApi, trayApi, traySettingsApi } from '../api/local'
import type { FleetJob, Job, PoolView } from '../api/types'
import { ErrorBox, LogView, StatePill, useAction, usePoll } from '../components/common'
import { fmtTime, isActive } from '../lib/format'

/** My jobs (submitted by this client), with logs and kill. */
export function ClientJobs() {
  const [all, setAll] = useState(false)
  const jobs = usePoll(() => longarmApi.jobs({ mine: !all, limit: 50 }), 3000, [all])
  const [open, setOpen] = useState<FleetJob>()
  const { run, busy } = useAction()
  return (
    <div>
      <div className="row" style={{ marginBottom: 6 }}>
        <label className="row small">
          <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} /> everyone's jobs
        </label>
        <span className="spacer" />
        <span className="muted small">longarm jobs · longarm logs · longarm kill</span>
      </div>
      <ErrorBox error={jobs.error} />
      <table className="grid small" data-testid="my-jobs">
        <tbody>
          {(jobs.data ?? []).map((j) => (
            <tr key={`${j.worker}/${j.job.spec.id}`} onClick={() => setOpen(j)} style={{ cursor: 'pointer' }}>
              <td className="mono">{j.job.spec.name || j.job.spec.id.slice(-8)}</td>
              <td>{j.worker}</td>
              <td>
                <StatePill state={j.job.state} />
                {j.job.has_exit_code ? ` ${j.job.exit_code ?? 0}` : ''}
              </td>
              <td className="muted nowrap">{fmtTime(j.job.created_at)}</td>
              <td>
                {isActive(j.job.state) && (
                  <button
                    className="danger"
                    disabled={busy}
                    onClick={(e) => {
                      e.stopPropagation()
                      if (!confirm(killQuestion(j.job, j.worker))) return
                      run(() => longarmApi.kill(j.job.spec.id, j.worker), 'killed').then(jobs.reload)
                    }}
                  >
                    kill
                  </button>
                )}
              </td>
            </tr>
          ))}
          {jobs.data === undefined && !jobs.error && (
            <tr>
              <td className="notice">loading jobs…</td>
            </tr>
          )}
          {jobs.data !== undefined && jobs.data.length === 0 && (
            <tr>
              <td className="notice">{all ? 'no jobs' : 'no jobs of yours yet: longarm run <worker> -- <command>'}</td>
            </tr>
          )}
        </tbody>
      </table>
      {open && <JobLogs key={open.job.spec.id} j={open} />}
    </div>
  )
}

/** "Kill job build (alice) on box?": what a kill confirmation names. */
export function killQuestion(job: Job, worker?: string): string {
  const name = job.spec.name || job.spec.id.slice(-8)
  const owner = job.spec.owner || job.spec.submitted_by
  return `Kill job ${name}${owner ? ` (${owner})` : ''}${worker ? ` on ${worker}` : ''}? It stops at once; its output so far is kept.`
}

function JobLogs({ j }: { j: FleetJob }) {
  const openStream = useCallback(
    (onChunk: Parameters<typeof longarmApi.logs>[2], onDone: Parameters<typeof longarmApi.logs>[3]) => longarmApi.logs(j.job.spec.id, j.worker, onChunk, onDone),
    [j.job.spec.id, j.worker],
  )
  return (
    <section>
      <h3>
        {j.worker} · <span className="mono">{(j.job.spec.argv ?? []).join(' ').slice(0, 80)}</span>
      </h3>
      <LogView open={openStream} />
    </section>
  )
}

/** Port forwards hosted by longarm daemon (longarm forward --bg / --list / --stop). */
export function Forwards({ workers }: { workers: string[] }) {
  const fw = usePoll(longarmApi.forwards, 3000)
  const { run, busy } = useAction()
  const [worker, setWorker] = useState('')
  const [target, setTarget] = useState('')
  return (
    <div>
      <form
        className="row wrap"
        onSubmit={async (e) => {
          e.preventDefault()
          const r = await run(() => longarmApi.startForward(worker || workers[0], target), (f) => `forwarding ${f.local_addr}`)
          if (r) {
            setTarget('')
            fw.reload()
          }
        }}
      >
        <select aria-label="forward worker" value={worker || workers[0] || ''} onChange={(e) => setWorker(e.target.value)}>
          {workers.map((w) => (
            <option key={w}>{w}</option>
          ))}
        </select>
        <input aria-label="target port" placeholder="port or host:port on the worker" value={target} onChange={(e) => setTarget(e.target.value)} style={{ flex: 1 }} />
        <button className="primary" disabled={busy || !target || !workers.length}>
          Forward
        </button>
      </form>
      <ErrorBox error={fw.error} />
      <table className="grid small" style={{ marginTop: 8 }} data-testid="forwards">
        <tbody>
          {(fw.data ?? []).map((f) => (
            <tr key={f.local_addr}>
              <td>
                <button className="link" onClick={() => trayApi.open(f.url ?? '')}>
                  {f.local_addr}
                </button>
              </td>
              <td>
                → {f.worker}:{f.target}
              </td>
              <td className="muted">{f.connections ?? 0} conns</td>
              <td className="red small">{f.error}</td>
              <td>
                <button disabled={busy} onClick={() => run(() => longarmApi.stopForward(f.local_addr ?? ''), 'stopped').then(fw.reload)}>
                  stop
                </button>
              </td>
            </tr>
          ))}
          {(fw.data ?? []).length === 0 && (
            <tr>
              <td className="notice">no forwards</td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}

const POLICIES: [string, string][] = [
  ['race', 'race: dial every leg, first mTLS handshake wins (sticky winner)'],
  ['tailscale', 'prefer tailscale'],
  ['longnet', 'prefer longnet (hub relay + hole punching)'],
  ['tcp', 'prefer plain tcp'],
]

/** Settings: transport policy (longarm transport), notifications, this client (version, logs, telemetry, diagnostics). */
export function Settings({ pool, clientUp = true }: { pool?: PoolView; clientUp?: boolean }) {
  return (
    <div>
      {clientUp && <TransportPolicy />}
      <TraySettingsView pool={pool} clientUp={clientUp} />
    </div>
  )
}

function TransportPolicy() {
  const s = usePoll(longarmApi.settings, 0)
  const { run, busy } = useAction()
  const [choice, setChoice] = useState<string>()
  const current = choice ?? s.data?.transport ?? 'race'
  return (
    <section>
      <h3>Transport policy</h3>
      {POLICIES.map(([b, label]) => (
        <label key={b} className="row" style={{ margin: '4px 0' }}>
          <input
            type="radio"
            name="transport"
            value={b}
            checked={current === b}
            disabled={busy}
            onChange={() => {
              setChoice(b) // optimistic; the daemon's answer follows
              run(() => longarmApi.setTransport(b), `transport ${b}`).then((r) => setChoice(r?.transport ?? s.data?.transport))
            }}
          />
          {label}
        </label>
      ))}
      <p className="muted small">CLI: longarm transport {current}</p>
    </section>
  )
}

function TraySettingsView({ pool, clientUp }: { pool?: PoolView; clientUp: boolean }) {
  const st = usePoll(traySettingsApi.get, 0)
  const daemon = usePoll(() => (clientUp ? longarmApi.status() : Promise.resolve(undefined)), 0, [clientUp])
  const { run, busy } = useAction()
  const s = st.data
  const set = (v: Parameters<typeof traySettingsApi.set>[0], ok: string) => run(() => traySettingsApi.set(v), ok).then((r) => r && st.setData(r))
  return (
    <>
      <section>
        <h3>Notifications</h3>
        <ErrorBox error={st.error} />
        <label className="row" style={{ margin: '4px 0' }}>
          <input
            type="checkbox"
            aria-label="notify about my workers"
            checked={!!s && !s.mute_workers}
            disabled={!s || busy}
            onChange={(e) => set({ mute_workers: !e.target.checked }, e.target.checked ? 'worker notifications on' : 'worker notifications off')}
          />
          my workers: offline / back online, memory low, link relayed
        </label>
        <label className="row" style={{ margin: '4px 0' }}>
          <input
            type="checkbox"
            aria-label="notify about my jobs"
            checked={!!s && !s.mute_jobs}
            disabled={!s || busy}
            onChange={(e) => set({ mute_jobs: !e.target.checked }, e.target.checked ? 'job notifications on' : 'job notifications off')}
          />
          my jobs: done, failed, killed, lost
        </label>
        <p className="muted small">Only your own workers and the jobs you started notify; other people&apos;s machines never do.</p>
      </section>
      <section>
        <h3>This client</h3>
        <div className="small" data-testid="client-info">
          <div>
            longarm-tray {s?.version ?? '…'}
            {daemon.data?.version ? ` · longarm daemon ${daemon.data.version}` : ''}
          </div>
          <div>identity {pool?.identity ?? daemon.data?.identity ?? '—'}</div>
          <div>
            hub {pool?.hub?.address || '—'} {pool?.hub?.address ? (pool.hub.connected ? '(connected)' : `(down: ${pool.hub.error ?? ''})`) : ''}
          </div>
        </div>
        <label className="row" style={{ margin: '8px 0 4px' }}>
          <input
            type="checkbox"
            aria-label="telemetry"
            checked={!!s && !s.telemetry_disabled && !s.telemetry_env_off}
            disabled={!s || busy || !!s?.telemetry_env_off}
            onChange={(e) => set({ telemetry_disabled: !e.target.checked }, e.target.checked ? 'telemetry on' : 'telemetry off (queue dropped)')}
          />
          send anonymous telemetry (errors, timings; no names, commands or output){s?.telemetry_env_off ? ' - off: LONGARM_TELEMETRY=0' : ''}
        </label>
        <div className="row wrap" style={{ marginTop: 6 }}>
          {s?.client_logs && (
            <button disabled={busy} onClick={() => run(() => traySettingsApi.openFolder('client-logs'))}>
              Open logs folder
            </button>
          )}
          <button
            disabled={busy}
            title="longarm diag: logs, versions, configuration and a connectivity probe, with names and secrets removed; nothing is uploaded"
            onClick={() => run(() => traySettingsApi.collectDiag(), (r) => `diagnostics saved: ${r.zip}`)}
          >
            {busy ? 'Working…' : 'Collect diagnostics'}
          </button>
        </div>
      </section>
    </>
  )
}
