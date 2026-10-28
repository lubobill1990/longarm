import { useEffect, useState } from 'react'
import { longarmApi, trayApi, type TrayInfo } from '../api/local'
import type { PoolView } from '../api/types'
import { ErrorBox, FirewallWarning, Toasts, useAction } from '../components/common'
import { ClientJobs, Forwards, Settings } from './ClientTab'
import { PoolTab } from './PoolTab'
import { WorkerTab } from './WorkerTab'
import { AccountTab } from './AccountTab'
import { wailsMessage } from '../lib/wails'
import { ContextMenu, installAppBehaviour } from './appShell'

type Tab = 'pool' | 'jobs' | 'forwards' | 'worker' | 'settings' | 'account'

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
  const online = (pool?.workers ?? []).filter((w) => w.state === 'online').length
  return (
    <Toasts>
      <ContextMenu />
      <div className="tray">
        <TitleBar info={info} online={online} total={pool?.workers?.length ?? 0} />
        <div className="tabs" role="tablist">
          {tabs.map(([t, label]) => (
            <button key={t} role="tab" aria-selected={cur === t} className={cur === t ? 'active' : ''} onClick={() => setTab(t)}>
              {label}
            </button>
          ))}
        </div>
        <div className="body">
          <ErrorBox error={infoErr} />
          {needSignIn && cur !== 'account' && (
            <div className="warn-box" data-testid="signin-banner">
              Not signed in yet.{' '}
              <button className="primary" onClick={() => setTab('account')}>
                Sign in with Microsoft
              </button>
            </div>
          )}
          {info && info.roles.client && !info.client.running && cur !== 'worker' && cur !== 'account' && !needSignIn && <DaemonDown info={info} />}
          {cur !== 'worker' && <ClientFirewall pool={pool} />}
          {cur === 'pool' && <PoolTab pool={pool} error={poolErr} info={info} />}
          {cur === 'jobs' && clientUp && <ClientJobs />}
          {cur === 'forwards' && clientUp && <Forwards workers={(pool?.workers ?? []).map((w) => w.name)} />}
          {cur === 'settings' && clientUp && <Settings pool={pool} />}
          {cur === 'worker' && info && <WorkerTab info={info} />}
          {cur === 'account' && info && <AccountTab info={info} onChange={() => setReload((n) => n + 1)} />}
        </div>
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
  return (
    <div
      className="titlebar"
      onMouseDown={(e) => {
        // drag the frameless popup by its title bar (Wails)
        if (e.button === 0 && !(e.target as HTMLElement).closest('button')) wailsMessage('wails:drag')
      }}
    >
      <span className="title">Remote Workers</span>
      <span className="muted small" data-testid="online-count">
        {online}/{total} online
      </span>
      <span className="spacer" />
      <button
        className={`tbtn ${pinned ? 'on' : ''}`}
        title={pinned ? 'stays open until closed (click: hide when another window is clicked)' : 'hides when another window is clicked (click: keep it open)'}
        aria-label="pin"
        onClick={() => trayApi.pin(!pinned).then((r) => setPinned(r.pinned))}
      >
        {pinned ? '● pinned' : '○'}
      </button>
      <button className="tbtn" aria-label="close" title="close" onClick={() => trayApi.hide()}>
        ✕
      </button>
    </div>
  )
}

function DaemonDown({ info }: { info: TrayInfo }) {
  const { run, busy } = useAction()
  return (
    <div className="error-box">
      longarm daemon is not running{info.client.error ? ` (${info.client.error})` : ''}.{' '}
      <button disabled={busy} onClick={() => run(() => trayApi.startDaemon(), 'longarm daemon started')}>
        Start it
      </button>
    </div>
  )
}
