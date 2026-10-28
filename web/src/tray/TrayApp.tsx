import { useEffect, useState, type KeyboardEvent, type ReactNode } from 'react'
import { longarmApi, trayApi, type TrayInfo } from '../api/local'
import type { PoolView } from '../api/types'
import { ErrorBox, FirewallWarning, Toasts, useAction } from '../components/common'
import { ClientJobs, Forwards, Settings } from './ClientTab'
import { PoolTab, SourceBar, useNow } from './PoolTab'
import { poolMachines } from '../lib/machines'
import { WorkerTab } from './WorkerTab'
import { AccountTab } from './AccountTab'
import { UpdateBanner } from './UpdateBanner'
import { wailsMessage } from '../lib/wails'
import { ContextMenu, installAppBehaviour } from './appShell'
import { IconChip, IconClose, IconForward, IconPin, IconServers, IconSettings, IconTerminal, IconUser } from '../components/icons'
import { useTheme } from './theme'
import { Banner } from './ui'
import './tokens.css'
import './tray.css'

type Tab = 'pool' | 'jobs' | 'forwards' | 'worker' | 'settings' | 'account'

const TAB_ICON: Record<Tab, (p: { size?: number }) => ReactNode> = {
  pool: IconServers,
  jobs: IconTerminal,
  forwards: IconForward,
  worker: IconChip,
  settings: IconSettings,
  account: IconUser,
}

/**
 * longarm-tray's popup: tabs by installed
 * role. The pool view streams from longarm daemon (/local/longarm/v1/pool/watch).
 */
export function TrayApp() {
  const [info, setInfo] = useState<TrayInfo>()
  const [infoErr, setInfoErr] = useState<string>()
  const [tab, setTab] = useState<Tab>(() => (new URLSearchParams(location.search).get('tab') as Tab) || 'pool')
  const [pool, setPool] = useState<PoolView>()
  const [poolErr, setPoolErr] = useState<string>()
  const [reload, setReload] = useState(0)
  const [theme, setTheme] = useTheme()
  // an app, not a browser page: links to the system browser, no file drops
  useEffect(() => installAppBehaviour(), [])
  useEffect(() => {
    const load = () =>
      trayApi
        .info()
        .then((i) => {
          setInfo(i)
          setInfoErr(undefined)
        })
        .catch((e) => setInfoErr(String(e.message ?? e)))
    load()
    const t = setInterval(load, 5000)
    return () => clearInterval(t)
  }, [reload])
  const clientUp = !!info?.client.running
  const polling = new URLSearchParams(location.search).has('poll')
  useEffect(() => {
    if (!clientUp) return
    if (polling) {
      // ?poll=1: plain requests instead of a stream (headless screenshots)
      const load = () =>
        longarmApi
          .pool()
          .then((v) => {
            setPool(v)
            setPoolErr(undefined)
          })
          .catch((e) => setPoolErr(String(e.message ?? e)))
      load()
      const t = setInterval(load, 2000)
      return () => clearInterval(t)
    }
    const s = longarmApi.watchPool(
      (v) => {
        setPool(v)
        setPoolErr(undefined)
      },
      (e) => e && setPoolErr(e),
    )
    return () => s.close()
  }, [clientUp])
  const tabs: [Tab, string][] = []
  if (!info || info.roles.client) tabs.push(['pool', 'Workers'], ['jobs', 'My jobs'], ['forwards', 'Forwards'])
  if (info?.roles.worker) tabs.push(['worker', 'This worker'])
  if (!info || info.roles.client) tabs.push(['settings', 'Settings'])
  if (info?.account) tabs.push(['account', 'Account'])
  // not signed in yet: the account tab (Sign in with Microsoft) first
  const needSignIn = !!info?.account?.needed
  const asked = new URLSearchParams(location.search).has('tab')
  const want = needSignIn && !asked && tab === 'pool' ? 'account' : tab
  const cur = tabs.some(([t]) => t === want) ? want : (tabs[0]?.[0] ?? 'pool')
  const now = useNow()
  // machines, not workers: a session worker and its admin worker count once
  const machines = poolMachines(pool?.workers ?? [], now)
  const online = machines.active.filter((m) => m.state === 'online').length
  // tabs as a roving tab stop: arrows, Home and End move between them
  const onTabKey = (e: KeyboardEvent<HTMLButtonElement>) => {
    const i = tabs.findIndex(([t]) => t === cur)
    const next = { ArrowRight: i + 1, ArrowDown: i + 1, ArrowLeft: i - 1, ArrowUp: i - 1, Home: 0, End: tabs.length - 1 }[e.key]
    if (next === undefined || !tabs.length) return
    e.preventDefault()
    const [t] = tabs[(next + tabs.length) % tabs.length]
    setTab(t)
    document.getElementById(`tab-${t}`)?.focus()
  }
  const curLabel = tabs.find(([t]) => t === cur)?.[1]
  // the tab bar scrolls sideways if the labels don't fit (wide fonts): keep the current tab in view
  useEffect(() => {
    document.getElementById(`tab-${cur}`)?.scrollIntoView?.({ block: 'nearest', inline: 'nearest' })
  }, [cur])
  return (
    <Toasts>
      <ContextMenu />
      <div className="tray">
        {/* fixed: the title bar, the tabs (a rail in a wide window) and the pool's source line; only .body scrolls */}
        <header className="tray-head" data-testid="tray-head">
          <TitleBar info={info} online={online} total={machines.active.length + machines.offline.length} />
        </header>
        <nav className="tray-nav" aria-label="Sections">
          <div className="tabs" role="tablist">
            {tabs.map(([t, label]) => {
              const Icon = TAB_ICON[t]
              return (
                <button
                  key={t}
                  id={`tab-${t}`}
                  role="tab"
                  aria-selected={cur === t}
                  aria-controls="tray-panel"
                  tabIndex={cur === t ? 0 : -1}
                  className={`tab ${cur === t ? 'active' : ''}`}
                  onClick={() => setTab(t)}
                  onKeyDown={onTabKey}
                >
                  <Icon size={16} />
                  <span className="tab-label">{label}</span>
                </button>
              )
            })}
          </div>
        </nav>
        <main className="tray-main">
          {cur === 'pool' && pool && <SourceBar pool={pool} now={now} />}
          <div className="body" id="tray-panel" role="tabpanel" aria-labelledby={`tab-${cur}`}>
            <div className="page">
              <h1 className="page-title">{curLabel}</h1>
              <div className="banners">
                <ErrorBox error={infoErr} />
                <UpdateBanner />
                {needSignIn && cur !== 'account' && (
                  <Banner
                    tone="warn"
                    testId="signin-banner"
                    action={
                      <button className="primary" onClick={() => setTab('account')}>
                        Sign in with Microsoft
                      </button>
                    }
                  >
                    Not signed in yet.
                  </Banner>
                )}
                {info && info.roles.client && !info.client.running && cur !== 'worker' && cur !== 'account' && !needSignIn && <DaemonDown info={info} />}
                {cur !== 'worker' && <ClientFirewall pool={pool} />}
              </div>
              {cur === 'pool' && <PoolTab pool={pool} error={poolErr} info={info} onNavigate={(t) => setTab(t as Tab)} />}
              {cur === 'jobs' && clientUp && <ClientJobs />}
              {cur === 'forwards' && clientUp && <Forwards workers={(pool?.workers ?? []).map((w) => w.name)} />}
              {cur === 'settings' && <Settings pool={pool} clientUp={clientUp} theme={theme} onTheme={setTheme} />}
              {cur === 'worker' && info && <WorkerTab info={info} onNavigate={(t) => setTab(t as Tab)} />}
              {cur === 'account' && info && <AccountTab info={info} onChange={() => setReload((n) => n + 1)} />}
            </div>
          </div>
        </main>
      </div>
    </Toasts>
  )
}

/**
 * The daemon's firewall problem: a Block rule always (it defeats every
 * direct path), a missing allow rule once legs are relayed (as longarm pool).
 */
function ClientFirewall({ pool }: { pool?: PoolView }) {
  const fw = pool?.firewall
  const relayed = relayedWorkers(pool)
  if (!fw || fw.ok || (!(fw.blocking ?? []).length && !relayed.length)) return null
  return <FirewallWarning fw={fw} relayed={relayed} />
}

/** Workers with a leg that is up over a relay ("wa (longnet relay-udp)"). */
function relayedWorkers(pool?: PoolView): string[] {
  const relayKinds = ['relay', 'derp', 'peer-relay']
  return (pool?.workers ?? []).flatMap((w) => {
    const legs = (w.link?.legs ?? []).filter((l) => l.up && relayKinds.includes(l.kind ?? '')).map((l) => l.label)
    return legs.length ? [`${w.name} (${legs.join(', ')})`] : []
  })
}

function TitleBar({ info, online, total }: { info?: TrayInfo; online: number; total: number }) {
  const [pinned, setPinned] = useState(false)
  useEffect(() => {
    if (info) setPinned(info.pinned)
  }, [info?.pinned]) // eslint-disable-line react-hooks/exhaustive-deps
  const allUp = total > 0 && online === total
  return (
    <div
      className="titlebar"
      onMouseDown={(e) => {
        // drag the frameless popup by its title bar (Wails)
        if (e.button === 0 && !(e.target as HTMLElement).closest('button')) wailsMessage('wails:drag')
      }}
    >
      <span className="brand-mark" aria-hidden="true" />
      <span className="title">Remote Workers</span>
      <span className={`count-pill ${total === 0 ? '' : allUp ? 'ok' : online ? 'warn' : 'bad'}`}>
        <span className="count-dot" aria-hidden="true" />
        <span data-testid="online-count">
          {online}/{total} online
        </span>
      </span>
      <span className="spacer" />
      <button
        className={`icon-btn ${pinned ? 'on' : ''}`}
        title={pinned ? 'stays open until closed (click: hide when another window is clicked)' : 'hides when another window is clicked (click: keep it open)'}
        aria-label="pin"
        aria-pressed={pinned}
        onClick={() => trayApi.pin(!pinned).then((r) => setPinned(r.pinned))}
      >
        <IconPin />
      </button>
      <button className="icon-btn" aria-label="close" title="close" onClick={() => trayApi.hide()}>
        <IconClose />
      </button>
    </div>
  )
}

function DaemonDown({ info }: { info: TrayInfo }) {
  const { run, busy } = useAction()
  return (
    <Banner
      tone="error"
      testId="daemon-down"
      action={
        <button disabled={busy} onClick={() => run(() => trayApi.startDaemon(), 'longarm daemon started')}>
          Start it
        </button>
      }
    >
      longarm daemon is not running{info.client.error ? ` (${info.client.error})` : ''}.
    </Banner>
  )
}
