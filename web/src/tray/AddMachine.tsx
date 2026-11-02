import { useState } from 'react'
import { trayApi, type AddMachineInfo } from '../api/local'
import { usePoll } from '../components/common'
import { IconCheck, IconExternal } from '../components/icons'
import { copyText } from './AccountTab'
import { Banner, Section, Skeleton } from './ui'

/**
 * "Add a machine" (the Workers tab's + button): the commands that install
 * Longarm on another machine and sign it in to this machine's hub - the
 * installer from the GitHub Release of the hub's newest stable version
 * (/trayapi/addmachine), for the roles and the OS picked here. A hub that
 * signs in with Microsoft's own tenant downloads from the Microsoft-internal
 * copy of the repository (github.com/gim-home/longarm, the same releases), which needs
 * a GitHub sign-in: gh release download.
 */

export type Platform = 'windows' | 'macos' | 'deb' | 'rpm'
export type Arch = 'amd64' | 'arm64'

export interface AddOptions {
  worker: boolean
  admin: boolean
  client: boolean
  platform: Platform
  arch: Arch
  /** download from the internal copy (gim-home) */
  internal: boolean
}

export interface Step {
  title: string
  /** the shell: PowerShell or a POSIX shell */
  shell: 'powershell' | 'sh'
  cmd: string
}

const PLATFORMS: [Platform, string][] = [
  ['windows', 'Windows'],
  ['macos', 'macOS'],
  ['deb', 'Linux (deb)'],
  ['rpm', 'Linux (rpm)'],
]

/** The architectures' names on each platform. */
function archLabel(p: Platform, a: Arch): string {
  if (p === 'macos') return a === 'arm64' ? 'Apple silicon' : 'Intel'
  return a === 'arm64' ? 'ARM64' : 'x64'
}

/** The release asset: longarm-windows-amd64.msi, longarm-darwin-arm64.pkg, longarm-linux-amd64.deb ... */
export function assetName(p: Platform, a: Arch): string {
  switch (p) {
    case 'windows':
      return `longarm-windows-${a}.msi`
    case 'macos':
      return `longarm-darwin-${a}.pkg`
    default:
      return `longarm-linux-${a}.${p}`
  }
}

/** The roles' value: ROLES= of the MSI, --role of longarmd login ("" = nothing picked). */
export function rolesOf(o: AddOptions): '' | 'client' | 'worker' | 'both' {
  if (o.worker && o.client) return 'both'
  if (o.worker) return 'worker'
  if (o.client) return 'client'
  return ''
}

/** The release page (a browser download; the internal copy needs a GitHub sign-in with access). */
export function releaseURL(repo: string, version?: string): string {
  return version ? `https://${repo}/releases/tag/v${version}` : `https://${repo}/releases/latest`
}

/** The commands for the other machine, in order; [] when no role is picked or no hub is known. */
export function installSteps(o: AddOptions, info: AddMachineInfo): Step[] {
  const roles = rolesOf(o)
  const hub = info.hub_host
  if (!roles || !hub) return []
  const repo = o.internal ? info.internal_repo : info.public_repo
  const asset = assetName(o.platform, o.arch)
  const v = info.version
  const url = v ? `https://${repo}/releases/download/v${v}/${asset}` : `https://${repo}/releases/latest/download/${asset}`
  // the internal copy is private: gh with a GitHub sign-in that can read it
  const gh = (dir: string) => `gh release download ${v ? `v${v} ` : ''}-R ${repo} -p ${asset} -D ${dir} --clobber`
  const steps: Step[] = []
  if (o.platform === 'windows') {
    const props = [`MODE=custom`, `ROLES=${roles}`, `HUB=${hub}`, `LOGIN=1`]
    if (o.admin && o.worker) props.push('ADMIN_WORKER=1')
    const download = o.internal ? gh('$env:TEMP') : `Invoke-WebRequest -UseBasicParsing ${url} -OutFile $msi`
    steps.push({
      title: 'Download and install (PowerShell; it asks for administrator rights)',
      shell: 'powershell',
      cmd: [`$msi = Join-Path $env:TEMP '${asset}'`, download, `Start-Process msiexec.exe -Verb RunAs -Wait -ArgumentList "/i \`"$msi\`" /qb ${props.join(' ')}"`].join('\n'),
    })
    steps.push({
      title: 'Sign in: the Longarm tray opens and shows a code - approve it in any browser',
      shell: 'powershell',
      cmd: `# or by hand:\n${roles === 'client' ? `longarm login ${hub}` : `longarmd login ${hub} --role ${roles}`}`,
    })
    return steps
  }
  const file = `/tmp/${asset}`
  const download = o.internal ? gh('/tmp') : `curl -fsSL -o ${file} ${url}`
  const install =
    o.platform === 'macos' ? `sudo installer -pkg ${file} -target /` : o.platform === 'deb' ? `sudo apt-get install -y ${file}` : `sudo dnf install -y ${file}`
  steps.push({ title: 'Download and install', shell: 'sh', cmd: `${download}\n${install}` })
  if (roles === 'client') {
    steps.push({ title: 'Sign in (a code to approve in any browser)', shell: 'sh', cmd: `longarm login ${hub}` })
    return steps
  }
  const start = o.platform === 'macos' ? 'sudo longarmd service install --user "$USER" && sudo longarmd service start' : 'sudo systemctl enable --now "longarmd@$USER"'
  steps.push({
    title: `Sign in${roles === 'both' ? ' (one code for the worker and the client)' : ''}, then start the worker`,
    shell: 'sh',
    cmd: `longarmd login ${hub} --role ${roles}\n${start}`,
  })
  return steps
}

/** A best guess of the other machine's platform: this one's. */
function guessPlatform(): Platform {
  const ua = typeof navigator === 'undefined' ? '' : navigator.userAgent
  if (/Mac OS X|Macintosh/.test(ua)) return 'macos'
  if (/Linux/.test(ua) && !/Android/.test(ua)) return 'deb'
  return 'windows'
}

export function AddMachine({ onClose }: { onClose: () => void }) {
  const st = usePoll(() => trayApi.addMachine(), 5 * 60_000)
  const info = st.data
  const [opts, setOpts] = useState<Omit<AddOptions, 'internal'>>({ worker: true, admin: false, client: false, platform: guessPlatform(), arch: 'amd64' })
  const [source, setSource] = useState<'auto' | 'public' | 'internal'>('auto')
  const set = (n: Partial<AddOptions>) =>
    setOpts((o) => {
      const r = { ...o, ...n }
      if (!r.worker || r.platform !== 'windows') r.admin = false // the admin worker: Windows, with a worker
      return r
    })
  const internal = source === 'auto' ? !!info?.internal : source === 'internal'
  const full: AddOptions = { ...opts, internal }
  const steps = info ? installSteps(full, info) : []
  const repo = info ? (internal ? info.internal_repo : info.public_repo) : ''
  return (
    <div className="addm" data-testid="add-machine">
      <div className="addm-head">
        <button className="link" onClick={onClose} data-testid="add-machine-back">
          ← Workers
        </button>
        <h2>Add a machine</h2>
      </div>
      <p className="muted small">
        Run these on the other machine: they install Longarm{info?.version ? ` ${info.version}` : ''} and sign it in to {info?.hub_host ? <b className="mono">{info.hub_host}</b> : 'this hub'} with your account, so it shows up here.
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
          <Section title="The machine will">
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
          <Section title="Its system">
            <div className="addm-row">
              <div className="segmented" role="radiogroup" aria-label="Operating system">
                {PLATFORMS.map(([p, label]) => (
                  <label key={p} className={opts.platform === p ? 'is-active' : ''}>
                    <input type="radio" name="addm-os" className="sr-only" value={p} checked={opts.platform === p} onChange={() => set({ platform: p })} />
                    {label}
                  </label>
                ))}
              </div>
              <div className="segmented" role="radiogroup" aria-label="Architecture">
                {(['amd64', 'arm64'] as Arch[]).map((a) => (
                  <label key={a} className={opts.arch === a ? 'is-active' : ''}>
                    <input type="radio" name="addm-arch" className="sr-only" value={a} checked={opts.arch === a} onChange={() => set({ arch: a })} />
                    {archLabel(opts.platform, a)}
                  </label>
                ))}
              </div>
            </div>
          </Section>
          <Section
            title="Commands"
            aside={
              <select aria-label="Download from" data-testid="addm-source" value={source} onChange={(e) => setSource(e.target.value as typeof source)}>
                <option value="auto">{info.internal ? 'Microsoft-internal (this hub)' : 'GitHub (this hub)'}</option>
                <option value="public">{info.public_repo}</option>
                <option value="internal">{info.internal_repo}</option>
              </select>
            }
          >
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
            {internal && (
              <div className="muted small" data-testid="addm-internal-note">
                {repo} is Microsoft-internal: <span className="mono">gh</span> needs a GitHub sign-in that can read it (<span className="mono">gh auth login</span>), or download{' '}
                <span className="mono">{assetName(opts.platform, opts.arch)}</span> from the release page in a signed-in browser.
              </div>
            )}
            <button className="link" onClick={() => trayApi.open(releaseURL(repo, info.version))} data-testid="addm-release">
              Release page <IconExternal size={12} />
            </button>
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
