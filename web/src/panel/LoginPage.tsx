import { useEffect, useState } from 'react'
import { hubApi } from '../api/hub'
import { ErrorBox } from '../components/common'

/**
 * The panel's sign-in: "Sign in with Microsoft" first when the hub has
 * Entra sign-in (GET /auth/info), the panel account (user, password, TOTP)
 * folded under it; without Entra the panel account form only.
 */
export function LoginPage({ onLogin }: { onLogin: () => void }) {
  const [entra, setEntra] = useState<boolean>()
  useEffect(() => {
    let alive = true
    hubApi
      .authInfo()
      .then((i) => alive && setEntra(!!i.entra))
      .catch(() => alive && setEntra(false))
    return () => {
      alive = false
    }
  }, [])
  if (entra === undefined) return <div className="notice">loading…</div>
  if (!entra) return <PasswordForm onLogin={onLogin} autoFocus />
  const next = location.pathname === '/login' ? '/' : location.pathname + location.search
  return (
    <div className="login card" data-testid="login-entra">
      <h1>longarm-hub</h1>
      <a className="button primary" href={`/auth/login?next=${encodeURIComponent(next)}`} data-testid="signin-microsoft">
        Sign in with Microsoft
      </a>
      <details style={{ marginTop: 16 }}>
        <summary className="muted">Panel account</summary>
        <PasswordForm onLogin={onLogin} nested />
      </details>
    </div>
  )
}

function PasswordForm({ onLogin, autoFocus, nested }: { onLogin: () => void; autoFocus?: boolean; nested?: boolean }) {
  const [user, setUser] = useState('')
  const [password, setPassword] = useState('')
  const [totp, setTotp] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  return (
    <form
      className={nested ? 'login-form' : 'login card'}
      style={nested ? { display: 'flex', flexDirection: 'column', gap: 8, marginTop: 8 } : undefined}
      onSubmit={async (e) => {
        e.preventDefault()
        setBusy(true)
        setError(undefined)
        try {
          await hubApi.login(user, password, totp.replace(/\s/g, ''))
          onLogin()
        } catch (err) {
          setError(err instanceof Error ? err.message : String(err))
          setTotp('')
        } finally {
          setBusy(false)
        }
      }}
    >
      {!nested && <h1>longarm-hub</h1>}
      <input aria-label="user" placeholder="user" autoComplete="username" value={user} onChange={(e) => setUser(e.target.value)} autoFocus={autoFocus} />
      <input
        aria-label="password"
        type="password"
        placeholder="password"
        autoComplete="current-password"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
      />
      <input
        aria-label="TOTP code"
        placeholder="TOTP code"
        autoComplete="one-time-code"
        inputMode="numeric"
        value={totp}
        onChange={(e) => setTotp(e.target.value)}
      />
      <ErrorBox error={error} />
      <button className="primary" disabled={busy || !user || !password || !totp}>
        Log in
      </button>
    </form>
  )
}
