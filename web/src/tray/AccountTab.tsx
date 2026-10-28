import { useEffect, useState } from 'react'
import {
  trayApi,
  type Account,
  type AccountRoles,
  type RolesState,
  type SignInState,
  type SignInTargets,
  type TrayInfo,
} from '../api/local'
import { ErrorBox, useAction } from '../components/common'

/**
 * longarm-tray's account tab: "Sign in with Microsoft" (one code, one
 * approval for this device, the worker and the admin worker; it starts by
 * itself after an install), then who is signed in, the worker's state,
 * what this computer does (send jobs / run jobs / admin commands, changed
 * with one administrator prompt and one approval), Sign out, Open panel.
 */
export function AccountTab({ info, onChange }: { info: TrayInfo; onChange?: () => void }) {
  const acct = info.account
  const [st, setSt] = useState<SignInState>()
  const [err, setErr] = useState<string>()
  const running = st?.state === 'running'
  useEffect(() => {
    let stop = false
    const load = () =>
      trayApi
        .signinState()
        .then((s) => {
          if (stop) return
          setSt((prev) => {
            if (prev?.state === 'running' && s.state !== 'running') onChange?.()
            return s
          })
          setErr(undefined)
        })
        .catch((e) => !stop && setErr(String(e.message ?? e)))
    load()
    const t = setInterval(load, running ? 1000 : 3000)
    return () => {
      stop = true
      clearInterval(t)
    }
  }, [running]) // eslint-disable-line react-hooks/exhaustive-deps
  if (!acct) return <div className="muted">This longarm-tray has no sign-in (update it).</div>
  return (
    <div data-testid="account-tab">
      <ErrorBox error={err} />
      {running && st ? (
        <SignInProgress st={st} />
      ) : (
        <>
          {st?.state === 'done' && st.result && (
            <div className="ok-box" data-testid="signin-done">
              {st.result.summary}
            </div>
          )}
          {(st?.state === 'error' || st?.state === 'canceled') && (
            <div className="error-box" data-testid="signin-error">
              {st.state === 'canceled' ? 'Sign-in canceled.' : `Sign-in failed: ${st.error ?? ''}`}
            </div>
          )}
          {acct.needed ? <SignInCard acct={acct} onStarted={setSt} /> : <AccountCard acct={acct} info={info} onChange={onChange} />}
        </>
      )}
      {acct.roles && <RolesCard acct={acct} cur={acct.roles} busy={running} onChange={onChange} />}
      {acct.admin && <AdminCard admin={acct.admin} onChange={onChange} />}
    </div>
  )
}

export function targetText(t: SignInTargets) {
  const parts: string[] = []
  if (t.client) parts.push('this device (acts as you)')
  if (t.worker) parts.push('your worker on this machine')
  if (t.admin_worker) parts.push('the admin worker (runs as SYSTEM)')
  return parts.join(', ')
}

/** "Mon 2 Nov, 14:05" */
export function when(iso?: string) {
  if (!iso) return ''
  const d = new Date(iso)
  return d.toLocaleString(undefined, { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' })
}

function SignInCard({ acct, onStarted }: { acct: Account; onStarted: (s: SignInState) => void }) {
  const { run, busy } = useAction()
  const [hub, setHub] = useState('')
  const [labels, setLabels] = useState('')
  const [more, setMore] = useState(false)
  const needHub = !acct.hub
  return (
    <section className="wcard" data-testid="signin-card">
      <h3>{acct.expired ? 'Sign in again' : 'Sign in with Microsoft'}</h3>
      {acct.expired && (
        <div className="warn-box" data-testid="signin-expired">
          Your sign-in expired on {when(acct.expires)}: sign in again.
        </div>
      )}
      <p className="small">
        One code and one approval in your browser sign in {targetText(acct.targets)}
        {acct.hub ? (
          <>
            {' '}
            at <span className="mono">{acct.hub}</span>
          </>
        ) : null}
        .
      </p>
      {(needHub || more) && (
        <div className="form-grid">
          <label>hub</label>
          <input aria-label="hub address" placeholder={acct.hub || 'longarm.example.com'} value={hub} onChange={(e) => setHub(e.target.value)} />
          {acct.targets.worker && (
            <>
              <label>worker labels</label>
              <input aria-label="worker labels" placeholder="pool=devbox (optional)" value={labels} onChange={(e) => setLabels(e.target.value)} />
            </>
          )}
        </div>
      )}
      <p className="row">
        <button
          className="primary"
          data-testid="signin-start"
          disabled={busy || (needHub && !hub)}
          onClick={async () => {
            const r = await run(() => trayApi.signinStart(hub, labels))
            if (r) onStarted(r)
          }}
        >
          Sign in with Microsoft
        </button>
        {!needHub && !more && (
          <button className="link" onClick={() => setMore(true)}>
            another hub, labels…
          </button>
        )}
      </p>
      {acct.run_mode === 'session' && acct.targets.worker && (
        <p className="muted small">
          The worker runs as you while you are logged on (also with the remote desktop disconnected); it stops at logoff or reboot until you log on
          again.
        </p>
      )}
    </section>
  )
}

/**
 * The running sign-in: the code large (also on the clipboard: the tray
 * copies it), Copy, the /link page. The popup stays open meanwhile.
 */
function SignInProgress({ st }: { st: SignInState }) {
  const { run, busy } = useAction()
  const [copied, setCopied] = useState(false)
  const c = st.code
  return (
    <section className="wcard" data-testid="signin-progress">
      <h3>Sign in with Microsoft</h3>
      {c ? (
        <>
          <p className="small">
            Your browser opened{' '}
            <a
              href={c.url}
              onClick={(e) => {
                e.preventDefault()
                trayApi.open(c.url)
              }}
            >
              {c.url}
            </a>
            . Sign in with Microsoft there and enter this code:
          </p>
          <div className="signin-code mono" data-testid="signin-code">
            {c.code}
          </div>
          <p className="row">
            <button
              className="primary"
              data-testid="signin-copy"
              onClick={() =>
                copyText(c.code).then(
                  () => setCopied(true),
                  () => setCopied(false),
                )
              }
            >
              {copied ? 'Copied' : 'Copy code'}
            </button>
            <button onClick={() => trayApi.open(c.url)}>Open the page again</button>
          </p>
          <p className="muted small">
            The code is also on your clipboard. Check that the page names this machine ({c.host}) and what it signs in: {targetText(st.targets)}
            {c.admin_worker ? '. The admin worker runs commands as SYSTEM' : ''}. The code expires at {new Date(c.expires).toLocaleTimeString()}. This
            window stays open until you are signed in.
          </p>
        </>
      ) : (
        <p className="muted small">Contacting the hub {st.hub}…</p>
      )}
      <ul className="small muted" data-testid="signin-steps">
        {(st.steps ?? []).slice(-5).map((s, i) => (
          <li key={i}>{s}</li>
        ))}
      </ul>
      <button className="danger" disabled={busy} onClick={() => run(() => trayApi.signinCancel())}>
        Cancel
      </button>
    </section>
  )
}

/** Copies text: the clipboard API, else a selected textarea (older WebViews). */
export async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    return
  } catch {
    const ta = document.createElement('textarea')
    ta.value = text
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    ta.remove()
    if (!ok) throw new Error('copy failed')
  }
}

function AccountCard({ acct, info, onChange }: { acct: Account; info: TrayInfo; onChange?: () => void }) {
  const { run, busy } = useAction()
  return (
    <section className="wcard" data-testid="account-card">
      <div className="head">
        <span className="dot" style={{ background: acct.signed_in ? 'var(--green)' : 'var(--amber)' }} />
        <span className="name">{acct.signed_in ? `Signed in${acct.user ? ` as ${acct.user}` : ''}` : 'Not signed in'}</span>
      </div>
      {acct.hub && <div className="small muted">hub {acct.hub}</div>}
      {acct.signed_in && acct.expires && <div className="small muted">sign-in valid until {when(acct.expires)} (renews by itself)</div>}
      {info.roles.worker && (
        <div className="small" data-testid="worker-online">
          worker:{' '}
          {info.worker.running ? (
            <span className="green">running</span>
          ) : (
            <span className="amber">not running{info.worker.error ? ` (${info.worker.error})` : ''}</span>
          )}
          {acct.run_mode === 'session' && <span className="muted"> · runs as you while you are logged on</span>}
          {acct.run_mode === 'service' && <span className="muted"> · Windows service</span>}
        </div>
      )}
      <p className="row">
        {info.hub_url && <button onClick={() => trayApi.openPanel('/')}>Open panel</button>}
        {acct.signed_in && (
          <button
            className="danger"
            disabled={busy}
            onClick={() => {
              if (!confirm('Sign out? This device and your worker here leave the hub (running jobs keep running). The admin worker stays.')) return
              run(() => trayApi.signout(), 'signed out').then(() => onChange?.())
            }}
          >
            Sign out
          </button>
        )}
      </p>
    </section>
  )
}

/** What the change does, in words (shown before the administrator prompt). */
export function rolesChangeText(cur: AccountRoles, want: AccountRoles) {
  const parts: string[] = []
  if (want.client && !cur.client) parts.push('add the client (send jobs)')
  if (!want.client && cur.client) parts.push('remove the client: this device leaves the hub')
  if (want.worker && !cur.worker) parts.push('add your worker (runs jobs as you, starts at your logon)')
  if (!want.worker && cur.worker) parts.push('remove the worker: it leaves the hub, running jobs end')
  if (want.admin_worker && !cur.admin_worker) parts.push('add the admin worker (administrator commands as SYSTEM, only you)')
  if (!want.admin_worker && cur.admin_worker) parts.push('remove the admin worker')
  return parts
}

/**
 * "This computer: [x] send jobs [x] run jobs [ ] admin commands": a change
 * takes one administrator prompt (it changes what the installer recorded,
 * so uninstall and upgrades keep up), then one approval signs the added
 * roles in.
 */
function RolesCard({ acct, cur, busy, onChange }: { acct: Account; cur: AccountRoles; busy: boolean; onChange?: () => void }) {
  const [want, setWant] = useState<AccountRoles>(cur)
  const [st, setSt] = useState<RolesState>()
  const { run, busy: posting } = useAction()
  const applying = st?.state === 'applying'
  useEffect(() => {
    if (!applying) setWant(cur)
  }, [cur.client, cur.worker, cur.admin_worker]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (!applying) return
    const t = setInterval(
      () =>
        trayApi
          .rolesState()
          .then((s) => {
            setSt(s)
            if (s.state !== 'applying') onChange?.()
          })
          .catch(() => {}),
      1000,
    )
    return () => clearInterval(t)
  }, [applying]) // eslint-disable-line react-hooks/exhaustive-deps
  const legacyAdmin = cur.admin_worker && !cur.worker // an install from before "admin worker only with a worker"
  const changes = rolesChangeText(cur, want)
  const editable = acct.roles_editable && !busy && !applying
  const set = (k: keyof AccountRoles, v: boolean) => {
    const n = { ...want, [k]: v }
    if (k === 'worker' && !v && !legacyAdmin) n.admin_worker = false
    setWant(n)
  }
  return (
    <section className="wcard" data-testid="roles-card">
      <h3>This computer</h3>
      <label className="check">
        <input type="checkbox" checked={want.client} disabled={!editable} onChange={(e) => set('client', e.target.checked)} />
        <span>
          <b>Send jobs</b> (client): run commands on your workers from here
        </span>
      </label>
      <label className="check">
        <input type="checkbox" data-testid="role-worker" checked={want.worker} disabled={!editable} onChange={(e) => set('worker', e.target.checked)} />
        <span>
          <b>Run jobs</b> (worker): this computer runs your jobs, as you, from your logon (no password)
        </span>
      </label>
      <label className="check">
        <input
          type="checkbox"
          checked={want.admin_worker}
          disabled={!editable || (!want.worker && !legacyAdmin)}
          onChange={(e) => set('admin_worker', e.target.checked)}
        />
        <span>
          <b>Admin commands</b> (admin worker): run administrator commands remotely, even before anyone logs on (as SYSTEM, only you)
          {!want.worker && !legacyAdmin ? <span className="muted"> · needs "run jobs"</span> : null}
        </span>
      </label>
      {acct.cloud_pc && !cur.worker && (
        <div className="small muted" data-testid="cloud-pc-hint">
          This looks like a Cloud PC or Dev Box ({acct.cloud_pc}): such machines usually run jobs.
        </div>
      )}
      {acct.roles_note && <div className="small muted">{acct.roles_note}</div>}
      {st?.state === 'error' && <div className="error-box">{st.error}</div>}
      {st?.state === 'done' && st.message && <div className="small green">{st.message}</div>}
      {applying && <div className="small muted">Confirm the administrator prompt…</div>}
      {changes.length > 0 && !applying && (
        <>
          <p className="small">This will {changes.join('; ')}. An administrator prompt follows; then one approval in your browser signs in what was added.</p>
          <p className="row">
            <button
              className="primary"
              data-testid="roles-apply"
              disabled={!editable || posting || (!want.client && !want.worker)}
              onClick={() => run(() => trayApi.rolesChange(want)).then((r) => r && setSt(r))}
            >
              Apply
            </button>
            <button onClick={() => setWant(cur)}>Undo</button>
          </p>
        </>
      )}
    </section>
  )
}

function AdminCard({ admin, onChange }: { admin: NonNullable<Account['admin']>; onChange?: () => void }) {
  const { run, busy } = useAction()
  if (!admin.enabled) return null
  return (
    <section className="wcard" data-testid="admin-card">
      <div className="head">
        <span className="dot" style={{ background: admin.signed_in ? 'var(--green)' : 'var(--amber)' }} />
        <span className="name">Admin worker</span>
        <span className="sub">
          {admin.service} · {admin.state ?? 'unknown'}
        </span>
      </div>
      <div className="small muted">
        Runs as SYSTEM, also with nobody logged on. Only you can use it, by its name; it is never chosen automatically.
        {admin.pending ? ' Waiting for your sign-in.' : ''}
      </div>
      <ErrorBox error={admin.error} />
      <p className="row">
        <button
          className="danger"
          disabled={busy}
          onClick={() => {
            if (!confirm('Disable the admin worker? The hub revokes it and its service is removed (an administrator prompt follows).')) return
            run(() => trayApi.adminDisable(), (r) => r.output).then(() => onChange?.())
          }}
        >
          Disable admin worker
        </button>
      </p>
    </section>
  )
}
