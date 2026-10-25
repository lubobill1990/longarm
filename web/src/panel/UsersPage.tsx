import { useState } from 'react'
import { hubApi } from '../api/hub'
import type { Me } from '../api/types'
import { ErrorBox, useAction, usePoll } from '../components/common'
import { fmtTime } from '../lib/format'

/**
 * Panel accounts (password + TOTP), like longarm-hub user add/ls/rm. Logins
 * added here are administrators; logins of people (role user) are made
 * under People.
 */
export function UsersPage({ me }: { me: Me }) {
  const users = usePoll(hubApi.users, 0)
  const { run, busy } = useAction()
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [current, setCurrent] = useState('')
  const [made, setMade] = useState<{ name: string; totp_secret: string; otpauth: string }>()
  return (
    <div>
      <div className="page-title">
        <h1>Panel logins</h1>
      </div>
      <ErrorBox error={users.error} />
      <div className="cards">
        <div className="card">
          <table className="grid">
            <tbody>
              {(users.data ?? []).map((u) => (
                <tr key={u.name} data-testid={`user-${u.name}`}>
                  <td>
                    <b>{u.name}</b> {u.name === me.user && <span className="muted">(you)</span>}
                  </td>
                  <td>
                    <span className={`pill ${u.role === 'user' ? '' : 'info'}`}>{u.role || 'admin'}</span>
                    {u.role === 'user' && u.tenant && u.tenant !== u.name && <span className="muted small"> as {u.tenant}</span>}
                  </td>
                  <td className="small muted">created {fmtTime(u.created_at)}</td>
                  <td style={{ textAlign: 'right' }}>
                    {u.name !== me.user && (
                      <button className="danger" disabled={busy || !current} title={current ? '' : 'enter your password below first'} onClick={() => confirm(`Delete ${u.name}?`) && run(() => hubApi.deleteUser(u.name, current), `${u.name} deleted`).then(users.reload)}>
                        Delete
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <form
          className="card"
          onSubmit={async (e) => {
            e.preventDefault()
            const r = await run(() => hubApi.addUser(name, password, current))
            if (r) {
              setMade(r)
              setPassword('')
              users.reload()
            }
          }}
        >
          <div className="form-grid">
            <label>name</label>
            <input aria-label="new user name" value={name} onChange={(e) => setName(e.target.value)} />
            <label>password</label>
            <input aria-label="new user password" type="password" autoComplete="new-password" placeholder="at least 10 characters" value={password} onChange={(e) => setPassword(e.target.value)} />
            <label>your password</label>
            <input aria-label="your password" type="password" autoComplete="current-password" placeholder="required for account changes" value={current} onChange={(e) => setCurrent(e.target.value)} />
          </div>
          <p>
            <button className="primary" disabled={busy || !name || password.length < 10 || !current}>
              Add user
            </button>
          </p>
          <p className="muted small">Adds an administrator. People who should only see their own jobs get a login under People (“web panel login”).</p>
          {made && (
            <div data-testid="totp-secret">
              <p className="small">
                Add this TOTP secret for <b>{made.name}</b> to an authenticator app now; it is not shown again.
              </p>
              <div className="codebox">{made.totp_secret}</div>
              <div className="codebox small" style={{ marginTop: 6 }}>
                {made.otpauth}
              </div>
            </div>
          )}
        </form>
      </div>
    </div>
  )
}
