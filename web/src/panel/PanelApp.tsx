import { lazy, Suspense, useEffect, useState } from 'react'
import { hubApi, isAdmin } from '../api/hub'
import { ApiError } from '../api/http'
import type { Me } from '../api/types'
import { Toasts } from '../components/common'
import { feedbackURL } from '../lib/feedback'
import { Link, navigate, usePath } from '../lib/router'
import { AuditPage } from './AuditPage'
import { GetPage } from './GetPage'
import { JobsPage } from './JobsPage'
import { JoinCodesPage } from './JoinCodesPage'
import { LinksPage } from './LinksPage'
import { LoginPage } from './LoginPage'
import { FleetPage } from './FleetPage'
import { MachinesPage } from './MachinesPage'
import { MePage } from './MePage'
import { NodesPage } from './NodesPage'
import { PortLinksPage } from './PortLinksPage'
import { ProfilesPage } from './ProfilesPage'
import { TenantsPage } from './TenantsPage'
import { UsersPage } from './UsersPage'

// its own chunk (fonts, illustrations): the panel pages and the tray don't load it
const LandingPage = lazy(() => import('../landing/LandingPage'))

// Administrators see machines and jobs as their user does (Nodes, Jobs);
// the fleet as a whole only as counts (Fleet).
const ADMIN_NAV: [string, string][] = [
  ['/nodes', 'Nodes'],
  ['/fleet', 'Fleet'],
  ['/jobs', 'Jobs'],
  ['/joincodes', 'Add machine'],
  ['/tenants', 'People'],
  ['/profiles', 'Toolchains'],
  ['/links', 'Links'],
  ['/port-links', 'Port links'],
  ['/audit', 'Audit'],
  ['/users', 'Panel logins'],
]

// panel logins with role "user": their own (and team-shared) jobs, their
// machines, the workers (read-only), their port links, the installers and
// their quota
const USER_NAV: [string, string][] = [
  ['/jobs', 'My jobs'],
  ['/machines', 'My machines'],
  ['/nodes', 'Nodes'],
  ['/port-links', 'Port links'],
  ['/get', 'Get Longarm'],
  ['/me', 'Me'],
]

const home = (me: Me) => (isAdmin(me) ? '/nodes' : '/jobs')

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
  // signed in on /login (back from a provider's sign-in): into the hub
  useEffect(() => {
    if (me && path === '/login') navigate(home(me), { replace: true })
  }, [me, path])
  // the front page, signed in or not
  if (path === '/') {
    return (
      <Suspense fallback={null}>
        <LandingPage me={me} hubHome={me ? home(me) : '/login'} />
      </Suspense>
    )
  }
  if (me === undefined) return <div className="notice">loading…</div>
  if (me === null) {
    return (
      <Toasts>
        <LoginPage
          onLogin={() =>
            hubApi.me().then((m) => {
              setMe(m)
              if (path === '/login') navigate(home(m), { replace: true })
            })
          }
        />
      </Toasts>
    )
  }
  const admin = isAdmin(me)
  const nav = admin ? ADMIN_NAV : USER_NAV
  const page = path === '/login' ? home(me) : '/' + path.split('/')[1]
  const allowed = page === '/me' || page === '/get' || nav.some(([to]) => to === page)
  return (
    <Toasts>
      <div className="shell">
        <header className="topbar">
          <Link to="/" className="brand-link">
            <img className="brand-mark" src="/favicon.png" alt="" aria-hidden="true" />
            <span className="brand">Longarm</span>
          </Link>
          <span className="muted small">
            {me.hub} · {me.version}
          </span>
          <nav>
            {nav.map(([to, label]) => (
              <Link key={to} to={to} className={page === to ? 'active' : ''}>
                {label}
              </Link>
            ))}
          </nav>
          <span className="spacer" />
          <a href={feedbackURL(me.feedback_url)} target="_blank" rel="noreferrer" className="muted" data-testid="panel-feedback">
            Feedback
          </a>
          <Link to="/me" className="muted">
            {me.user}
            {!admin && me.tenant && me.tenant !== me.user ? ` (${me.tenant})` : ''}
          </Link>
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
          {!allowed && <div className="notice">This page is for administrators only.</div>}
          {page === '/nodes' && <NodesPage admin={admin} />}
          {page === '/jobs' && <JobsPage path={path} me={me} />}
          {page === '/port-links' && <PortLinksPage admin={admin} />}
          {page === '/me' && <MePage me={me} />}
          {page === '/get' && <GetPage me={me} />}
          {!admin && page === '/machines' && <MachinesPage me={me} />}
          {admin && page === '/fleet' && <FleetPage />}
          {admin && page === '/joincodes' && <JoinCodesPage me={me} />}
          {admin && page === '/tenants' && <TenantsPage me={me} />}
          {admin && page === '/profiles' && <ProfilesPage />}
          {admin && page === '/links' && <LinksPage />}
          {admin && page === '/audit' && <AuditPage />}
          {admin && page === '/users' && <UsersPage me={me} />}
        </main>
      </div>
    </Toasts>
  )
}
