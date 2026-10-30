import { Fragment, useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { localUpdateApi, longarmApi, trayApi, traySettingsApi } from '../api/local'
import type { Job, PoolView } from '../api/types'
import { ErrorBox, LogView, StatePill, useAction, usePoll } from '../components/common'
import { IconForward, IconTerminal } from '../components/icons'
import { fmtTime, isActive } from '../lib/format'
import { AgentSkill } from './AgentSkill'
import type { ThemePref } from './theme'
import { EmptyState, Section, Skeleton } from './ui'

/**
 * My jobs: the jobs this computer started (longarm jobs --mine, what the
 * job notifications are about), with their output and kill. There is no
 * "everyone" switch: the hub shows a user only its own and team-shared jobs
 * (an administrator too: docs/security.md "What admins see"), so a wider
 * list would only add this user's jobs from other computers and other
 * people's shared ones; longarm jobs --all lists those.
 *
 * A job's details (command, times, output) open right under its row, so a
 * long list never sends you to the bottom for them; a second click closes.
 */
export function ClientJobs() {
  const jobs = usePoll(() => longarmApi.jobs({ mine: true, limit: 50 }), 3000)
  const [open, setOpen] = useState<string>()
  const { run, busy } = useAction()
  return (
    <div>
      <div className="toolbar">
        <span className="muted small" data-testid="jobs-scope">
          Started from this computer
        </span>
        <span className="spacer" />
        <span className="muted small mono" title="longarm jobs --all: every job you may see, from all your computers">
          longarm jobs · logs · kill
        </span>
      </div>
      <ErrorBox error={jobs.error} />
      <table className="grid list small" data-testid="my-jobs">
        <tbody>
          {(jobs.data ?? []).map((j) => {
            const key = `${j.worker}/${j.job.spec.id}`
            const name = j.job.spec.name || j.job.spec.id.slice(-8)
            const isOpen = open === key
            const toggle = () => setOpen(isOpen ? undefined : key)
            return (
              <Fragment key={key}>
                <tr
                  className={`clickable ${isOpen ? 'selected' : ''}`}
                  tabIndex={0}
                  aria-expanded={isOpen}
                  aria-label={`${name} on ${j.worker}: ${isOpen ? 'hide' : 'show'} its output`}
                  onClick={toggle}
                  onKeyDown={(e) => {
                    if ((e.key === 'Enter' || e.key === ' ') && e.target === e.currentTarget) {
                      e.preventDefault()
                      toggle()
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
                {isOpen && (
                  <JobDetailRow colSpan={5} job={j.job} worker={j.worker}>
                    <ClientJobLogs id={j.job.spec.id} worker={j.worker} />
                  </JobDetailRow>
                )}
              </Fragment>
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
                <EmptyState compact icon={<IconTerminal size={20} />} title="No jobs from this computer yet">
                  <span className="mono">longarm run &lt;worker&gt; -- &lt;command&gt;</span>
                </EmptyState>
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}

/** "Kill job build (alice) on box?": what a kill confirmation names. */
export function killQuestion(job: Job, worker?: string): string {
  const name = job.spec.name || job.spec.id.slice(-8)
  const owner = job.spec.owner || job.spec.submitted_by
  return `Kill job ${name}${owner ? ` (${owner})` : ''}${worker ? ` on ${worker}` : ''}? It stops at once; its output so far is kept.`
}

/**
 * A job's details as a row right under the job's own row (My jobs, Jobs on
 * this worker): the command, where and when, the reason, then its output
 * (children). It scrolls into view when it opens.
 */
export function JobDetailRow({ job, worker, colSpan, children }: { job: Job; worker?: string; colSpan: number; children: ReactNode }) {
  const ref = useRef<HTMLTableRowElement>(null)
  useEffect(() => {
    ref.current?.scrollIntoView?.({ block: 'nearest' })
  }, [])
  const owner = job.spec.owner || job.spec.submitted_by
  return (
    <tr className="row-detail" ref={ref} data-testid="job-detail">
      <td colSpan={colSpan}>
        <dl className="facts small">
          <dt>command</dt>
          <dd className="mono">{(job.spec.argv ?? []).join(' ') || '—'}</dd>
          {worker && (
            <>
              <dt>worker</dt>
              <dd>{worker}</dd>
            </>
          )}
          {owner && (
            <>
              <dt>owner</dt>
              <dd>{owner}</dd>
            </>
          )}
          <dt>started</dt>
          <dd>
            {fmtTime(job.created_at)}
            {job.ended_at ? ` · ended ${fmtTime(job.ended_at)}` : ''}
          </dd>
          {job.reason && (
            <>
              <dt>reason</dt>
              <dd>{job.reason}</dd>
            </>
          )}
          <dt>id</dt>
          <dd className="mono">{job.spec.id}</dd>
        </dl>
        {children}
      </td>
    </tr>
  )
}

function ClientJobLogs({ id, worker }: { id: string; worker: string }) {
  const openStream = useCallback(
    (onChunk: Parameters<typeof longarmApi.logs>[2], onDone: Parameters<typeof longarmApi.logs>[3]) => longarmApi.logs(id, worker, onChunk, onDone),
    [id, worker],
  )
  return <LogView open={openStream} />
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
      <AgentSkill />
      <AutoUpdate />
    </div>
  )
}

/** Release channels, most tested first (docs/ci-cd.md "Release channels"). */
export const CHANNELS: [string, string, string][] = [
  ['stable', 'Stable', 'releases only'],
  ['preview', 'Preview', 'release candidates too, before everybody gets them'],
  ['dev', 'Dev', 'every change merged to main: the newest, least tested'],
]

/**
 * Updates of this machine (the Longarm Update service of a Windows MSI
 * install; absent elsewhere): the release channel it follows (stable by
 * default) and automatic updates (off by default), both changed by the
 * machine's owner or an administrator only - never by the hub.
 */
export function AutoUpdate() {
  const st = usePoll(localUpdateApi.status, 0)
  const { run, busy } = useAction()
  const u = st.data
  if (!u?.service) return null
  const newer = u.available && u.available !== u.installed ? ` · ${u.available} available` : ''
  const channel = u.channel || 'stable'
  const owner = u.can_set_auto ? '' : " (only this machine's owner or an administrator can change this)"
  return (
    <Section card title="Updates" testId="auto-update">
      <div className="field-help">
        Longarm {u.installed ?? '…'}
        {newer}
      </div>
      <div className="field-inline">
        <div>
          <div className="field-label" id="channel-label">
            Release channel
          </div>
          <div className="field-help" data-testid="channel-help">
            {CHANNELS.find(([c]) => c === channel)?.[2] ?? channel}
            {owner}. Moving to a more tested channel takes effect with its next release.
          </div>
        </div>
        <div className="segmented" role="radiogroup" aria-labelledby="channel-label">
          {CHANNELS.map(([c, label]) => (
            <label key={c} className={channel === c ? 'is-active' : ''}>
              <input
                type="radio"
                name="channel"
                className="sr-only"
                value={c}
                checked={channel === c}
                disabled={busy || !u.can_set_auto}
                onChange={() => run(() => localUpdateApi.setChannel(c), `release channel: ${label}`).then((r) => r && st.setData(r))}
              />
              {label}
            </label>
          ))}
        </div>
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
            every {u.auto_interval_hours ?? 6} h, once no job started for 5 minutes, from the {channel} channel
            {owner}
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
    <Section card title="Appearance" testId="appearance">
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
    <Section card title="Transport policy" testId="transport">
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
      <Section card title="Notifications" testId="notifications">
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
      <Section card title="This client" testId="this-client">
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
