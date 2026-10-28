import { hubApi } from '../api/hub'
import type { Me } from '../api/types'
import { ErrorBox, usePoll } from '../components/common'
import { msiFile } from '../lib/release'
import { Link } from '../lib/router'

/** The one-line installs for this hub (the hub serves the scripts with its address and CA pinned). */
export function installLines(base: string) {
  const b = base.replace(/\/+$/, '')
  return {
    windows: `$env:LONGARM_LOGIN=1; $env:LONGARM_ROLE='both'; irm ${b}/install.ps1 | iex`,
    unix: `curl -fsSL ${b}/install.sh | sudo sh -s -- --login --role both`,
  }
}

/** "Get Longarm": the MSI of the newest release, the one-liners, three steps. */
export function GetPage({ me }: { me: Me }) {
  const rel = usePoll(() => hubApi.latestRelease(), 0)
  const base = me.base_url || location.origin
  const lines = installLines(base)
  const msi = msiFile(rel.data, 'amd64')
  const msiArm = msiFile(rel.data, 'arm64')
  return (
    <div style={{ maxWidth: 820 }}>
      <div className="page-title">
        <h1>Get Longarm</h1>
        {rel.data?.version && <span className="muted">version {rel.data.version}</span>}
      </div>
      <ErrorBox error={rel.error} />
      <ol className="steps" data-testid="get-steps">
        <li>
          <b>Install</b> on each machine you want to use.
          <div style={{ marginTop: 6 }}>
            Windows:{' '}
            {msi ? (
              <a className="button primary" href={msi} data-testid="get-msi">
                Download the MSI (x64)
              </a>
            ) : (
              <span className="muted" data-testid="get-no-msi">
                this hub has no MSI release yet - use the one-liner below
              </span>
            )}
            {msiArm && (
              <>
                {' '}
                <a className="button" href={msiArm}>
                  ARM64
                </a>
              </>
            )}
            <div className="muted small" style={{ marginTop: 4 }}>
              Run it, keep <b>Default</b>, pick “Use this computer” or “Also run jobs on this computer”, Install.
            </div>
          </div>
          <div style={{ marginTop: 8 }}>
            Or one line - Windows (PowerShell):
            <div className="codebox" data-testid="get-ps1">
              {lines.windows}
            </div>
            Linux / macOS:
            <div className="codebox" data-testid="get-sh">
              {lines.unix}
            </div>
          </div>
        </li>
        <li>
          <b>Sign in with Microsoft.</b> The tray (or the install script) shows a code: open the link it shows in any browser, sign in, enter the code and
          approve the machine - once, for both the device and the worker. Already installed? <span className="mono">longarmd login --role both</span>.
        </li>
        <li>
          <b>Use it.</b> Your machines appear under <Link to="/machines">My machines</Link>; run something with{' '}
          <span className="mono">longarm run &lt;machine&gt; -- &lt;command&gt;</span> and follow it under <Link to="/jobs">My jobs</Link>.
        </li>
      </ol>
      <p className="muted small">
        Updates: the tray shows “Update to …” when this hub has a newer release, and <span className="mono">longarm</span> prints a notice once a day.
      </p>
    </div>
  )
}
