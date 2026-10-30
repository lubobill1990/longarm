import { useEffect, useState } from 'react'
import { hubApi, type AuthProvider } from '../api/hub'
import { ErrorBox } from '../components/common'

/**
 * The panel's sign-in: one button per identity provider of the hub (GET
 * /auth/info: Microsoft work or school accounts, personal Microsoft
 * accounts, Google, Apple) with the panel account (user, password, TOTP)
 * folded under them; without providers the panel account form only.
 */
export function LoginPage({ onLogin }: { onLogin: () => void }) {
  const [providers, setProviders] = useState<AuthProvider[]>()
  useEffect(() => {
    let alive = true
    hubApi
      .authInfo()
      .then((i) => alive && setProviders(i.providers ?? (i.entra ? [{ id: '', label: 'Microsoft' }] : [])))
      .catch(() => alive && setProviders([]))
    return () => {
      alive = false
    }
  }, [])
  if (providers === undefined) return <div className="notice">loading…</div>
  if (!providers.length) return <PasswordForm onLogin={onLogin} autoFocus />
  const next = encodeURIComponent(location.pathname === '/login' ? '/' : location.pathname + location.search)
  return (
    <div className="login card" data-testid="login-entra">
      <h1>longarm-hub</h1>
      {providers.map((p, i) => (
        <a
          key={p.id}
          className={i === 0 ? 'button primary' : 'button'}
          style={i > 0 ? { marginTop: 8 } : undefined}
          // an older hub (no provider list): its one Microsoft sign-in
          href={p.id ? `/auth/login?provider=${p.id}&next=${next}` : `/auth/login?next=${next}`}
          data-testid={!p.id || p.id === 'entra' ? 'signin-microsoft' : p.id === 'microsoft' ? 'signin-microsoft-account' : `signin-${p.id}`}
        >
          Sign in with {p.label}
        </a>
      ))}
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
