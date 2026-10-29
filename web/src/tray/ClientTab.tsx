import { useCallback, useState } from 'react'
import { localUpdateApi, longarmApi, trayApi, traySettingsApi } from '../api/local'
import type { FleetJob, Job, PoolView } from '../api/types'
import { ErrorBox, LogView, StatePill, useAction, usePoll } from '../components/common'
import { IconForward, IconTerminal } from '../components/icons'
import { fmtTime, isActive } from '../lib/format'
import type { ThemePref } from './theme'
import { EmptyState, Section, Skeleton } from './ui'

/** My jobs (submitted by this client), with logs and kill. */
export function ClientJobs() {
  const [all, setAll] = useState(false)
  const jobs = usePoll(() => longarmApi.jobs({ mine: !all, limit: 50 }), 3000, [all])
  const [open, setOpen] = useState<FleetJob>()
  const { run, busy } = useAction()
  return (
    <div>
      <div className="toolbar">
        <label className="switch-row inline small">
          <input type="checkbox" className="switch" checked={all} onChange={(e) => setAll(e.target.checked)} /> everyone&apos;s jobs
        </label>
        <span className="spacer" />
        <span className="muted small mono">longarm jobs · logs · kill</span>
      </div>
      <ErrorBox error={jobs.error} />
      <table className="grid list small" data-testid="my-jobs">
        <tbody>
          {(jobs.data ?? []).map((j) => {
            const name = j.job.spec.name || j.job.spec.id.slice(-8)
            return (
              <tr
                key={`${j.worker}/${j.job.spec.id}`}
                className={`clickable ${open?.job.spec.id === j.job.spec.id ? 'selected' : ''}`}
                tabIndex={0}
                aria-label={`${name} on ${j.worker}: show its output`}
                onClick={() => setOpen(j)}
                onKeyDown={(e) => {
                  if ((e.key === 'Enter' || e.key === ' ') && e.target === e.currentTarget) {
                    e.preventDefault()
                    setOpen(j)
                  }
                }}
              >
                <td className="mono strong">{name}</td>
                <td className="muted">{j.worker}</td>
                <td className="nowrap">
                  <StatePill state={j.job.state} />
                  {j.job.has_exit_code ? <span className="mono muted"> {j.job.exit_code ?? 0}</span> : ''}
                </td>
                <td className="muted nowrap hide-narrow">{fmtTime(j.job.created_at)}</td>
                <td className="actions-cell">
                  {isActive(j.job.state) && (
                    <button
                      className="danger sm"
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
            )
          })}
          {jobs.data === undefined && !jobs.error && (
            <tr className="placeholder">
              <td colSpan={5}>
                <Skeleton lines={3} label="loading jobs…" />
              </td>
            </tr>
          )}
          {jobs.data !== undefined && jobs.data.length === 0 && (
            <tr className="placeholder">
              <td colSpan={5}>
                <EmptyState compact icon={<IconTerminal size={20} />} title={all ? 'No jobs' : 'No jobs of yours yet'}>
                  {all ? 'Nobody has run a job yet.' : <span className="mono">longarm run &lt;worker&gt; -- &lt;command&gt;</span>}
                </EmptyState>
              </td>
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
    <Section
      className="logs"
      title={
        <>
          {j.worker} · <span className="mono">{(j.job.spec.argv ?? []).join(' ').slice(0, 80)}</span>
        </>
      }
    >
      <LogView open={openStream} />
    </Section>
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
        className="forward-form"
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
        <input aria-label="target port" placeholder="port or host:port on the worker" value={target} onChange={(e) => setTarget(e.target.value)} />
        <button className="primary" disabled={busy || !target || !workers.length}>
          Forward
        </button>
      </form>
      <ErrorBox error={fw.error} />
      <table className="grid list small" data-testid="forwards">
        <tbody>
          {(fw.data ?? []).map((f) => (
            <tr key={f.local_addr}>
              <td className="dot-cell">
                <span className="dot" style={{ background: f.error ? 'var(--red)' : 'var(--green)' }} />
              </td>
              <td>
                <div className="fw-addr">
                  <button className="link mono" title="open in the browser" onClick={() => trayApi.open(f.url ?? '')}>
                    {f.local_addr}
                  </button>
                  <span className="fw-target">
                    → {f.worker}:{f.target}
                  </span>
                </div>
              </td>
              <td className="muted nowrap">{f.connections ?? 0} conns</td>
              <td>
                {f.error && (
                  <span className="pill bad" title={f.error}>
                    {f.error}
                  </span>
                )}
              </td>
              <td className="actions-cell">
                <button className="sm" disabled={busy} onClick={() => run(() => longarmApi.stopForward(f.local_addr ?? ''), 'stopped').then(fw.reload)}>
                  stop
                </button>
              </td>
            </tr>
          ))}
          {fw.data === undefined && !fw.error && (
            <tr className="placeholder">
              <td colSpan={5}>
                <Skeleton lines={2} label="loading forwards…" />
              </td>
            </tr>
          )}
          {fw.data !== undefined && fw.data.length === 0 && (
            <tr className="placeholder">
              <td colSpan={5}>
                <EmptyState compact icon={<IconForward size={20} />} title="No forwards">
                  A worker&apos;s port on this computer: pick a worker and a port above, or <span className="mono">longarm forward</span>.
                </EmptyState>
              </td>
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

/** Settings: appearance, transport policy (longarm transport), notifications, this client (version, logs, telemetry, diagnostics). */
export function Settings({
  pool,
  clientUp = true,
  theme,
  onTheme,
}: {
  pool?: PoolView
  clientUp?: boolean
  theme?: ThemePref
  onTheme?: (t: ThemePref) => Promise<void>
}) {
  return (
    <div className="settings">
      {theme && onTheme && <Appearance theme={theme} onTheme={onTheme} />}
      {clientUp && <TransportPolicy />}
      <TraySettingsView pool={pool} clientUp={clientUp} />
      <AutoUpdate />
    </div>
  )
}

/**
 * Automatic updates of this machine (the Longarm Update service of a
 * Windows MSI install; absent elsewhere): off by default, changed by the
 * machine's owner or an administrator only - never by the hub.
 */
export function AutoUpdate() {
  const st = usePoll(localUpdateApi.status, 0)
  const { run, busy } = useAction()
  const u = st.data
  if (!u?.service) return null
  const newer = u.available && u.available !== u.installed ? ` · ${u.available} available` : ''
  return (
    <Section title="Updates" testId="auto-update">
      <div className="field-help">
        Longarm {u.installed ?? '…'}
        {newer}
      </div>
      <label className="switch-row">
        <input
          type="checkbox"
          className="switch"
          aria-label="automatic updates"
          checked={!!u.auto}
          disabled={busy || !u.can_set_auto}
          onChange={(e) =>
            run(() => localUpdateApi.setAuto(e.target.checked), e.target.checked ? 'automatic updates on' : 'automatic updates off').then(
              (r) => r && st.setData(r),
            )
          }
        />
        <span>
          <span className="field-label">Install new releases automatically</span>
          <span className="field-help">
            every {u.auto_interval_hours ?? 6} h, once no job started for 5 minutes
            {!u.can_set_auto ? ' (only this machine\'s owner or an administrator can change this)' : ''}
          </span>
        </span>
      </label>
      {u.last && (
        <p className="field-help" data-testid="last-update">
          last update: {u.last.from} → {u.last.to}: {u.last.ok ? 'installed' : `failed (${u.last.error ?? ''})`}
        </p>
      )}
    </Section>
  )
}

const THEMES: [ThemePref, string][] = [
  ['system', 'System'],
  ['light', 'Light'],
  ['dark', 'Dark'],
]

/** System / Light / Dark: a segmented choice (radio buttons: the arrow keys move it). */
function Appearance({ theme, onTheme }: { theme: ThemePref; onTheme: (t: ThemePref) => Promise<void> }) {
  const { run } = useAction()
  return (
    <Section title="Appearance" testId="appearance">
      <div className="field-inline">
        <div>
          <div className="field-label" id="theme-label">
            Theme
          </div>
          <div className="field-help">System follows Windows&apos; light or dark app mode.</div>
        </div>
        <div className="segmented" role="radiogroup" aria-labelledby="theme-label">
          {THEMES.map(([t, label]) => (
            <label key={t} className={theme === t ? 'is-active' : ''}>
              <input type="radio" name="theme" className="sr-only" value={t} checked={theme === t} onChange={() => run(() => onTheme(t))} />
              {label}
            </label>
          ))}
        </div>
      </div>
    </Section>
  )
}

function TransportPolicy() {
  const s = usePoll(longarmApi.settings, 0)
  const { run, busy } = useAction()
  const [choice, setChoice] = useState<string>()
  const current = choice ?? s.data?.transport ?? 'race'
  return (
    <Section title="Transport policy">
      <div className="radio-list">
        {POLICIES.map(([b, label]) => (
          <label key={b} className={`radio-row ${current === b ? 'is-checked' : ''}`}>
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
            <span className="radio-label">{label}</span>
          </label>
        ))}
      </div>
      <p className="muted small mono">CLI: longarm transport {current}</p>
    </Section>
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
      <Section title="Notifications">
        <ErrorBox error={st.error} />
        <label className="switch-row">
          <input
            type="checkbox"
            className="switch"
            aria-label="notify about my workers"
            checked={!!s && !s.mute_workers}
            disabled={!s || busy}
            onChange={(e) => set({ mute_workers: !e.target.checked }, e.target.checked ? 'worker notifications on' : 'worker notifications off')}
          />
          <span>
            <span className="field-label">My workers</span>
            <span className="field-help">offline / back online, memory low, link relayed</span>
          </span>
        </label>
        <label className="switch-row">
          <input
            type="checkbox"
            className="switch"
            aria-label="notify about my jobs"
            checked={!!s && !s.mute_jobs}
            disabled={!s || busy}
            onChange={(e) => set({ mute_jobs: !e.target.checked }, e.target.checked ? 'job notifications on' : 'job notifications off')}
          />
          <span>
            <span className="field-label">My jobs</span>
            <span className="field-help">done, failed, killed, lost</span>
          </span>
        </label>
        <p className="muted small">Only your own workers and the jobs you started notify; other people&apos;s machines never do.</p>
      </Section>
      <Section title="This client">
        <dl className="facts small" data-testid="client-info">
          <dt>longarm-tray</dt>
          <dd className="mono">
            {s?.version ?? '…'}
            {daemon.data?.version ? ` · longarm daemon ${daemon.data.version}` : ''}
          </dd>
          <dt>identity</dt>
          <dd className="mono">{pool?.identity ?? daemon.data?.identity ?? '—'}</dd>
          <dt>hub</dt>
          <dd>
            <span className="mono">{pool?.hub?.address || '—'}</span>{' '}
            {pool?.hub?.address &&
              (pool.hub.connected ? (
                <span className="pill ok">connected</span>
              ) : (
                <span className="pill bad" title={pool.hub.error}>
                  down{pool.hub.error ? `: ${pool.hub.error}` : ''}
                </span>
              ))}
          </dd>
        </dl>
        <label className="switch-row">
          <input
            type="checkbox"
            className="switch"
            aria-label="telemetry"
            checked={!!s && !s.telemetry_disabled && !s.telemetry_env_off}
            disabled={!s || busy || !!s?.telemetry_env_off}
            onChange={(e) => set({ telemetry_disabled: !e.target.checked }, e.target.checked ? 'telemetry on' : 'telemetry off (queue dropped)')}
          />
          <span>
            <span className="field-label">Send anonymous telemetry</span>
            <span className="field-help">
              errors, timings; no names, commands or output{s?.telemetry_env_off ? ' - off: LONGARM_TELEMETRY=0' : ''}
            </span>
          </span>
        </label>
        <div className="button-row">
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
      </Section>
    </>
  )
}
