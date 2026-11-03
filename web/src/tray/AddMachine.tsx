import { useState } from 'react'
import { trayApi, type AddMachineInfo } from '../api/local'
import { usePoll } from '../components/common'
import { IconCheck } from '../components/icons'
import { copyText } from './AccountTab'
import { Banner, Section, Skeleton } from './ui'

/**
 * "Add a machine" (the Workers tab's + button): the one command that
 * installs Longarm on another machine and signs it in to this machine's hub
 * - the hub's own installer, as on its home page (irm <hub>/install.ps1 |
 * iex, curl <hub>/install.sh | sh): it downloads the hub's release from the
 * hub itself (no GitHub, no sign-in there), picks the architecture and the
 * package by itself, and shows a code to approve in any browser. Picked
 * here: what the machine will do and whether it runs Windows.
 */

export type Platform = 'windows' | 'unix'

export interface AddOptions {
  worker: boolean
  admin: boolean
  client: boolean
  platform: Platform
}

export interface Step {
  title: string
  /** the shell: PowerShell or a POSIX shell */
  shell: 'powershell' | 'sh'
  cmd: string
}

const PLATFORMS: [Platform, string][] = [
  ['windows', 'Windows'],
  ['unix', 'macOS / Linux'],
]

/** The roles' value: LONGARM_ROLE / --role of the installer ("" = nothing picked). */
export function rolesOf(o: AddOptions): '' | 'client' | 'worker' | 'both' {
  if (o.worker && o.client) return 'both'
  if (o.worker) return 'worker'
  if (o.client) return 'client'
  return ''
}

/** The command for the other machine (the hub's installer); [] when no role is picked or no hub is known. */
export function installSteps(o: AddOptions, info: AddMachineInfo): Step[] {
  const roles = rolesOf(o)
  const hub = info.hub_host
  if (!roles || !hub) return []
  const base = `https://${hub}`
  if (o.platform === 'windows') {
    const env = ['$env:LONGARM_LOGIN=1', `$env:LONGARM_ROLE='${roles}'`]
    if (o.admin && o.worker) env.push("$env:LONGARM_MSI_PROPS='ADMIN_WORKER=1'")
    return [
      {
        title: 'In PowerShell: it asks for administrator rights, installs, and shows a code to approve in any browser',
        shell: 'powershell',
        cmd: `${env.join('; ')}; irm ${base}/install.ps1 | iex`,
      },
    ]
  }
  return [
    {
      title: 'In a terminal: it asks for your password, installs, and shows a code to approve in any browser',
      shell: 'sh',
      cmd: `curl -fsSL ${base}/install.sh | sudo sh -s -- --login --role ${roles}`,
    },
  ]
}

/** A best guess of the other machine's platform: this one's. */
function guessPlatform(): Platform {
  const ua = typeof navigator === 'undefined' ? '' : navigator.userAgent
  return /Mac OS X|Macintosh|Linux/.test(ua) && !/Android/.test(ua) ? 'unix' : 'windows'
}

export function AddMachine({ onClose }: { onClose: () => void }) {
  const st = usePoll(() => trayApi.addMachine(), 5 * 60_000)
  const info = st.data
  const [opts, setOpts] = useState<AddOptions>({ worker: true, admin: false, client: false, platform: guessPlatform() })
  const set = (n: Partial<AddOptions>) =>
    setOpts((o) => {
      const r = { ...o, ...n }
      if (!r.worker || r.platform !== 'windows') r.admin = false // the admin worker: Windows, with a worker
      return r
    })
  const steps = info ? installSteps(opts, info) : []
  return (
    <div className="addm" data-testid="add-machine">
      <div className="addm-head">
        <button className="link" onClick={onClose} data-testid="add-machine-back">
          ← Workers
        </button>
        <h2>Add a machine</h2>
      </div>
      <p className="muted small">
        Run this on the other machine: it installs Longarm{info?.version ? ` ${info.version}` : ''} and signs it in to{' '}
        {info?.hub_host ? <b className="mono">{info.hub_host}</b> : 'this hub'} with your account, so it shows up here.
      </p>
      {st.error && !info && <Banner tone="error">{st.error}</Banner>}
      {!info && !st.error && <Skeleton lines={4} label="asking the hub…" />}
      {info && !info.hub_host && (
        <Banner tone="warn" testId="add-machine-nohub">
          This machine has no hub yet: sign in on the Account tab first.
        </Banner>
      )}
      {info?.hub_host && (
        <>
          <Section card title="The machine will">
            <label className="check">
              <input type="checkbox" data-testid="addm-worker" checked={opts.worker} onChange={(e) => set({ worker: e.target.checked })} />
              <span>
                <b>Run jobs</b> (worker): your jobs run there, as you
              </span>
            </label>
            <label className="check">
              <input
                type="checkbox"
                data-testid="addm-admin"
                checked={opts.admin}
                disabled={!opts.worker || opts.platform !== 'windows'}
                onChange={(e) => set({ admin: e.target.checked })}
              />
              <span>
                <b>Admin commands</b> (admin worker): administrator commands, even before anyone logs on (as SYSTEM, only you)
                {opts.platform !== 'windows' ? <span className="muted"> · Windows only</span> : !opts.worker ? <span className="muted"> · needs "run jobs"</span> : null}
              </span>
            </label>
            <label className="check">
              <input type="checkbox" data-testid="addm-client" checked={opts.client} onChange={(e) => set({ client: e.target.checked })} />
              <span>
                <b>Send jobs</b> (client): run commands on your workers from there
              </span>
            </label>
          </Section>
          <Section card title="Its system">
            <div className="segmented" role="radiogroup" aria-label="Operating system">
              {PLATFORMS.map(([p, label]) => (
                <label key={p} className={opts.platform === p ? 'is-active' : ''}>
                  <input type="radio" name="addm-os" className="sr-only" value={p} checked={opts.platform === p} onChange={() => set({ platform: p })} />
                  {label}
                </label>
              ))}
            </div>
          </Section>
          <Section card title="Command">
            {steps.length === 0 ? (
              <div className="muted small" data-testid="addm-none">
                Pick what the machine will do.
              </div>
            ) : (
              <ol className="addm-steps" data-testid="addm-steps">
                {steps.map((s, i) => (
                  <li key={i}>
                    <div className="addm-step-title">{s.title}</div>
                    <CommandBlock cmd={s.cmd} testId={`addm-cmd-${i}`} />
                  </li>
                ))}
              </ol>
            )}
          </Section>
        </>
      )}
    </div>
  )
}

function CommandBlock({ cmd, testId }: { cmd: string; testId?: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="cmdblock">
      <pre data-testid={testId}>{cmd}</pre>
      <button
        className="sm cmdblock-copy"
        aria-label="Copy"
        onClick={() =>
          copyText(cmd).then(
            () => {
              setCopied(true)
              setTimeout(() => setCopied(false), 1500)
            },
            () => setCopied(false),
          )
        }
      >
        {copied ? <IconCheck size={14} /> : 'Copy'}
      </button>
    </div>
  )
}
