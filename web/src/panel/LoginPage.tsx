import { useState } from 'react'
import { hubApi } from '../api/hub'
import { ErrorBox } from '../components/common'

export function LoginPage({ onLogin }: { onLogin: () => void }) {
  const [user, setUser] = useState('')
  const [password, setPassword] = useState('')
  const [totp, setTotp] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  return (
    <form
      className="login card"
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
      <h1>longarm-hub</h1>
      <input aria-label="user" placeholder="user" autoComplete="username" value={user} onChange={(e) => setUser(e.target.value)} autoFocus />
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
