import { useEffect, useState } from 'react'
import { trayApi, type Account, type SignInState, type SignInTargets, type TrayInfo } from '../api/local'
import { ErrorBox, useAction } from '../components/common'

/**
 * longarm-tray's account tab: "Sign in with Microsoft" (one code, one
 * approval for this device, the worker and the admin worker), then who is
 * signed in, the worker's state, Sign out, Open panel.
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
    const t = setInterval(load, running ? 1000 : 4000)
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
            <div className="small green" data-testid="signin-done">
              Signed in{st.result.user ? ` as ${st.result.user}` : ''}: {st.result.summary}
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

function SignInCard({ acct, onStarted }: { acct: Account; onStarted: (s: SignInState) => void }) {
  const { run, busy } = useAction()
  const [hub, setHub] = useState('')
  const [labels, setLabels] = useState('')
  const [more, setMore] = useState(false)
  const needHub = !acct.hub
  return (
    <section className="wcard" data-testid="signin-card">
      <h3>Sign in with Microsoft</h3>
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

function SignInProgress({ st }: { st: SignInState }) {
  const { run, busy } = useAction()
  const c = st.code
  return (
    <section className="wcard" data-testid="signin-progress">
      <h3>Sign in with Microsoft</h3>
      {c ? (
        <>
          <p className="small">
            Open{' '}
            <a
              href={c.url}
              onClick={(e) => {
                e.preventDefault()
                trayApi.open(c.url)
              }}
            >
              {c.url}
            </a>{' '}
            (opened in your browser), sign in with Microsoft and enter:
          </p>
          <div className="mono" data-testid="signin-code" style={{ fontSize: 28, letterSpacing: 3, margin: '8px 0' }}>
            {c.code}
          </div>
          <p className="muted small">
            Check that the page names this machine ({c.host}) and what it signs in: {targetText(st.targets)}
            {c.admin_worker ? '. The admin worker runs commands as SYSTEM' : ''}. The code expires at {new Date(c.expires).toLocaleTimeString()}.
          </p>
          <p className="row">
            <button onClick={() => trayApi.open(c.url)}>Open the page again</button>
            <button onClick={() => navigator.clipboard?.writeText(c.code)}>Copy code</button>
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

function AccountCard({ acct, info, onChange }: { acct: Account; info: TrayInfo; onChange?: () => void }) {
  const { run, busy } = useAction()
  return (
    <section className="wcard" data-testid="account-card">
      <div className="head">
        <span className="dot" style={{ background: acct.signed_in ? 'var(--green)' : 'var(--amber)' }} />
        <span className="name">{acct.signed_in ? `Signed in${acct.user ? ` as ${acct.user}` : ''}` : 'Not signed in'}</span>
      </div>
      {acct.hub && <div className="small muted">hub {acct.hub}</div>}
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
