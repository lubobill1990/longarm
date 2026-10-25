import { useEffect, useState } from 'react'
import { hubApi } from '../api/hub'
import { ApiError } from '../api/http'
import type { Me } from '../api/types'
import { Toasts } from '../components/common'
import { Link, navigate, usePath } from '../lib/router'
import { AuditPage } from './AuditPage'
import { JobsPage } from './JobsPage'
import { JoinCodesPage } from './JoinCodesPage'
import { LinksPage } from './LinksPage'
import { LoginPage } from './LoginPage'
import { NodesPage } from './NodesPage'
import { ProfilesPage } from './ProfilesPage'
import { UsersPage } from './UsersPage'

const NAV: [string, string][] = [
  ['/nodes', 'Nodes'],
  ['/jobs', 'Jobs'],
  ['/joincodes', 'Add machine'],
  ['/profiles', 'Toolchains'],
  ['/links', 'Links'],
  ['/audit', 'Audit'],
  ['/users', 'Users'],
]

/** The hub's web panel (fleet-wide). */
export function PanelApp() {
  const path = usePath()
  const [me, setMe] = useState<Me | null | undefined>(undefined) // undefined = loading, null = logged out
  useEffect(() => {
    hubApi
      .me()
      .then(setMe)
      .catch((e) => setMe(e instanceof ApiError && e.status === 401 ? null : null))
  }, [])
  if (me === undefined) return <div className="notice">loading…</div>
  if (me === null) {
    return (
      <Toasts>
        <LoginPage
          onLogin={() =>
            hubApi.me().then((m) => {
              setMe(m)
              if (path === '/' || path === '/login') navigate('/nodes')
            })
          }
        />
      </Toasts>
    )
  }
  const page = path === '/' || path === '/login' ? '/nodes' : '/' + path.split('/')[1]
  return (
    <Toasts>
      <div className="shell">
        <header className="topbar">
          <span className="brand">Longarm</span>
          <span className="muted small">
            {me.hub} · {me.version}
          </span>
          <nav>
            {NAV.map(([to, label]) => (
              <Link key={to} to={to} className={page === to ? 'active' : ''}>
                {label}
              </Link>
            ))}
          </nav>
          <span className="spacer" />
          <span className="muted">{me.user}</span>
          <button
            onClick={async () => {
              await hubApi.logout().catch(() => {})
              setMe(null)
            }}
          >
            Log out
          </button>
        </header>
        <main className="content">
          {page === '/nodes' && <NodesPage />}
          {page === '/jobs' && <JobsPage path={path} />}
          {page === '/joincodes' && <JoinCodesPage me={me} />}
          {page === '/profiles' && <ProfilesPage />}
          {page === '/links' && <LinksPage />}
          {page === '/audit' && <AuditPage />}
          {page === '/users' && <UsersPage me={me} />}
        </main>
      </div>
    </Toasts>
  )
}
